// Package aws wraps the subset of AWS APIs arbetern needs — currently only
// Cost Explorer. Credentials are resolved via the default AWS SDK v2 chain
// (environment variables, shared config/credentials files, EKS IRSA /
// EC2 IMDS), so the same binary works locally and in-cluster without any
// arbetern-specific plumbing.
//
// Cost Explorer is a global service but its endpoint lives in us-east-1; the
// region passed to NewClient only selects where the SigV4 signer points its
// signature, not what the data describes. Cost data is account-wide.
package aws

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

const (
	// DefaultRegion is where Cost Explorer is hosted. The SDK requires a
	// region even for global services; us-east-1 is the canonical choice.
	DefaultRegion = "us-east-1"

	// DefaultMetric is AmortizedCost, which spreads the up-front portion of
	// Reserved Instances and Savings Plans across the commitment term so a
	// daily cost report reflects the effective daily spend rather than a
	// $0 baseline punctuated by lumpy once-a-year charges. This is the
	// right default for any account that uses RIs or SPs. Callers can
	// still pick UnblendedCost (what the console shows by default),
	// BlendedCost, NetAmortizedCost, NetUnblendedCost, or UsageQuantity
	// via the `metric` argument.
	DefaultMetric = "AmortizedCost"

	// MaxDays caps single-call time windows to avoid surprising Cost
	// Explorer charges (each API call costs $0.01). 90 days is plenty
	// for weekly / monthly trend analysis.
	MaxDays = 90
)

// Client wraps the Cost Explorer SDK client.
type Client struct {
	ce     *costexplorer.Client
	region string
}

// NewClient creates an AWS client using the default credential chain. region
// controls where Cost Explorer calls are signed (not where data comes from);
// pass "" for the default us-east-1. A non-nil Client means credentials
// resolved, but the first API call is still the real health check.
func NewClient(ctx context.Context, region string) (*Client, error) {
	if region == "" {
		region = DefaultRegion
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	// Probe credentials so callers immediately know whether IRSA / env
	// creds are actually usable, rather than failing on the first tool
	// call several minutes later.
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("resolve AWS credentials: %w", err)
	}
	return &Client{
		ce:     costexplorer.NewFromConfig(cfg),
		region: region,
	}, nil
}

// Region returns the region used for signing Cost Explorer calls.
func (c *Client) Region() string { return c.region }

// --------------------------------------------------------------------------
// Cost & usage
// --------------------------------------------------------------------------

// CostAndUsageOpts are the arguments for GetCostAndUsage. All fields are
// optional; see defaults in GetCostAndUsage.
type CostAndUsageOpts struct {
	Start         string // YYYY-MM-DD, inclusive. Defaults to 8 days ago.
	End           string // YYYY-MM-DD, exclusive. Defaults to today.
	Granularity   string // DAILY (default), MONTHLY, or HOURLY.
	Metric        string // UnblendedCost (default), BlendedCost, AmortizedCost, NetUnblendedCost, NetAmortizedCost, UsageQuantity.
	GroupBy       string // "" (no grouping), SERVICE, LINKED_ACCOUNT, REGION, USAGE_TYPE, INSTANCE_TYPE, OPERATION, PURCHASE_TYPE, RECORD_TYPE, AVAILABILITY_ZONE, PLATFORM, TENANCY, DATABASE_ENGINE.
	ServiceFilter string // Exact AWS service name to filter to (e.g. "Amazon Elastic Compute Cloud - Compute"). Case-sensitive.
}

// CostPeriod is one granule (day, month, etc.) of cost data.
type CostPeriod struct {
	Start  string             `json:"start"`
	End    string             `json:"end"`
	Total  float64            `json:"total"`
	Unit   string             `json:"unit"`
	Groups map[string]float64 `json:"groups,omitempty"` // key: group key (service name, account id, region, etc.); empty if no grouping.
}

// CostAndUsageResult is the flattened response from Cost Explorer.
type CostAndUsageResult struct {
	Granularity string       `json:"granularity"`
	Metric      string       `json:"metric"`
	GroupBy     string       `json:"group_by,omitempty"`
	Start       string       `json:"start"`
	End         string       `json:"end"`
	Periods     []CostPeriod `json:"periods"`
}

// GetCostAndUsage queries Cost Explorer for per-period cost and returns a
// flattened result suitable for LLM consumption. Defaults: daily granularity,
// AmortizedCost metric, last 8 days (so workflows running on morning UTC
// get 7 full previous days + today-in-progress), no grouping.
func (c *Client) GetCostAndUsage(ctx context.Context, opts CostAndUsageOpts) (*CostAndUsageResult, error) {
	start, end, err := resolveDateRange(opts.Start, opts.End, 8)
	if err != nil {
		return nil, err
	}
	gran := strings.ToUpper(strings.TrimSpace(opts.Granularity))
	if gran == "" {
		gran = "DAILY"
	}
	switch gran {
	case "DAILY", "MONTHLY", "HOURLY":
	default:
		return nil, fmt.Errorf("invalid granularity %q (want DAILY, MONTHLY, HOURLY)", opts.Granularity)
	}
	metric := strings.TrimSpace(opts.Metric)
	if metric == "" {
		metric = DefaultMetric
	}
	validMetrics := map[string]bool{
		"UnblendedCost": true, "BlendedCost": true,
		"AmortizedCost": true, "NetAmortizedCost": true,
		"NetUnblendedCost": true, "UsageQuantity": true,
	}
	if !validMetrics[metric] {
		return nil, fmt.Errorf("invalid metric %q (want one of UnblendedCost, BlendedCost, AmortizedCost, NetAmortizedCost, NetUnblendedCost, UsageQuantity)", metric)
	}

	input := &costexplorer.GetCostAndUsageInput{
		TimePeriod: &cetypes.DateInterval{
			Start: awsv2.String(start),
			End:   awsv2.String(end),
		},
		Granularity: cetypes.Granularity(gran),
		Metrics:     []string{metric},
	}
	groupBy := strings.ToUpper(strings.TrimSpace(opts.GroupBy))
	if groupBy != "" {
		input.GroupBy = []cetypes.GroupDefinition{{
			Type: cetypes.GroupDefinitionTypeDimension,
			Key:  awsv2.String(groupBy),
		}}
	}
	if sf := strings.TrimSpace(opts.ServiceFilter); sf != "" {
		input.Filter = &cetypes.Expression{
			Dimensions: &cetypes.DimensionValues{
				Key:    cetypes.DimensionService,
				Values: []string{sf},
			},
		}
	}

	out, err := c.ce.GetCostAndUsage(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("GetCostAndUsage: %w", err)
	}

	result := &CostAndUsageResult{
		Granularity: gran,
		Metric:      metric,
		GroupBy:     groupBy,
		Start:       start,
		End:         end,
		Periods:     make([]CostPeriod, 0, len(out.ResultsByTime)),
	}
	for _, r := range out.ResultsByTime {
		p := CostPeriod{
			Start: awsv2.ToString(r.TimePeriod.Start),
			End:   awsv2.ToString(r.TimePeriod.End),
		}
		// Period total: prefer Total[metric]; if absent, sum Groups.
		if mv, ok := r.Total[metric]; ok {
			p.Total = parseAmount(awsv2.ToString(mv.Amount))
			p.Unit = awsv2.ToString(mv.Unit)
		}
		if len(r.Groups) > 0 {
			p.Groups = make(map[string]float64, len(r.Groups))
			for _, g := range r.Groups {
				if len(g.Keys) == 0 {
					continue
				}
				key := g.Keys[0]
				if mv, ok := g.Metrics[metric]; ok {
					amt := parseAmount(awsv2.ToString(mv.Amount))
					p.Groups[key] += amt
					if p.Unit == "" {
						p.Unit = awsv2.ToString(mv.Unit)
					}
				}
			}
			if p.Total == 0 {
				for _, v := range p.Groups {
					p.Total += v
				}
			}
		}
		result.Periods = append(result.Periods, p)
	}
	return result, nil
}

// --------------------------------------------------------------------------
// Cost forecast
// --------------------------------------------------------------------------

// ForecastOpts configures GetCostForecast.
type ForecastOpts struct {
	Start       string // YYYY-MM-DD, inclusive. Defaults to tomorrow.
	End         string // YYYY-MM-DD, exclusive. Defaults to 30 days from now.
	Granularity string // DAILY (default) or MONTHLY.
	Metric      string // UnblendedCost (default), BlendedCost, AmortizedCost, NetUnblendedCost, NetAmortizedCost, UsageQuantity.
}

// ForecastResult is the flattened forecast response.
type ForecastResult struct {
	Granularity string       `json:"granularity"`
	Metric      string       `json:"metric"`
	Start       string       `json:"start"`
	End         string       `json:"end"`
	Total       float64      `json:"total"`
	Unit        string       `json:"unit"`
	Periods     []CostPeriod `json:"periods"`
}

// GetCostForecast projects spend forward. Cost Explorer itself requires the
// start date to be >= today and <= 12 months from now; we default to
// tomorrow→+30 days which matches the console's "next month" view.
func (c *Client) GetCostForecast(ctx context.Context, opts ForecastOpts) (*ForecastResult, error) {
	today := time.Now().UTC()
	start := opts.Start
	if start == "" {
		start = today.AddDate(0, 0, 1).Format("2006-01-02")
	}
	end := opts.End
	if end == "" {
		end = today.AddDate(0, 0, 31).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", start); err != nil {
		return nil, fmt.Errorf("invalid start %q (want YYYY-MM-DD)", start)
	}
	if _, err := time.Parse("2006-01-02", end); err != nil {
		return nil, fmt.Errorf("invalid end %q (want YYYY-MM-DD)", end)
	}
	gran := strings.ToUpper(strings.TrimSpace(opts.Granularity))
	if gran == "" {
		gran = "DAILY"
	}
	if gran != "DAILY" && gran != "MONTHLY" {
		return nil, fmt.Errorf("forecast granularity must be DAILY or MONTHLY")
	}
	metric := strings.TrimSpace(opts.Metric)
	if metric == "" {
		metric = DefaultMetric
	}
	// Cost Explorer's GetCostForecast uses a DIFFERENT metric enum than
	// GetCostAndUsage: UPPER_SNAKE_CASE (AMORTIZED_COST) rather than
	// CamelCase (AmortizedCost). Translate the familiar CamelCase form
	// callers already use for GetCostAndUsage so the tool surface stays
	// consistent.
	forecastMetric, err := toForecastMetric(metric)
	if err != nil {
		return nil, err
	}

	in := &costexplorer.GetCostForecastInput{
		TimePeriod: &cetypes.DateInterval{
			Start: awsv2.String(start),
			End:   awsv2.String(end),
		},
		Granularity: cetypes.Granularity(gran),
		Metric:      forecastMetric,
	}
	out, err := c.ce.GetCostForecast(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("GetCostForecast: %w", err)
	}
	result := &ForecastResult{
		Granularity: gran,
		Metric:      metric,
		Start:       start,
		End:         end,
	}
	if out.Total != nil {
		result.Total = parseAmount(awsv2.ToString(out.Total.Amount))
		result.Unit = awsv2.ToString(out.Total.Unit)
	}
	for _, f := range out.ForecastResultsByTime {
		p := CostPeriod{
			Start: awsv2.ToString(f.TimePeriod.Start),
			End:   awsv2.ToString(f.TimePeriod.End),
			Total: parseAmount(awsv2.ToString(f.MeanValue)),
			Unit:  result.Unit,
		}
		result.Periods = append(result.Periods, p)
	}
	return result, nil
}

// --------------------------------------------------------------------------
// Dimension values (enumerate services, accounts, regions, ...)
// --------------------------------------------------------------------------

// DimensionValuesOpts configures GetDimensionValues.
type DimensionValuesOpts struct {
	Dimension string // required: SERVICE, LINKED_ACCOUNT, REGION, USAGE_TYPE, ... (see GroupBy enum).
	Start     string // YYYY-MM-DD inclusive. Defaults to 30 days ago.
	End       string // YYYY-MM-DD exclusive. Defaults to today.
	Search    string // Optional prefix filter. Empty returns all values for the window.
}

// DimensionValuesResult is the flattened response.
type DimensionValuesResult struct {
	Dimension string   `json:"dimension"`
	Start     string   `json:"start"`
	End       string   `json:"end"`
	Values    []string `json:"values"`
}

// GetDimensionValues enumerates values for a Cost Explorer dimension within
// the time window. Useful for discovering the exact service name strings
// (which are long and finicky — e.g. "Amazon Elastic Compute Cloud -
// Compute") before filtering GetCostAndUsage.
func (c *Client) GetDimensionValues(ctx context.Context, opts DimensionValuesOpts) (*DimensionValuesResult, error) {
	dim := strings.ToUpper(strings.TrimSpace(opts.Dimension))
	if dim == "" {
		return nil, fmt.Errorf("dimension is required (e.g. SERVICE, LINKED_ACCOUNT, REGION)")
	}
	start, end, err := resolveDateRange(opts.Start, opts.End, 30)
	if err != nil {
		return nil, err
	}
	in := &costexplorer.GetDimensionValuesInput{
		Dimension: cetypes.Dimension(dim),
		TimePeriod: &cetypes.DateInterval{
			Start: awsv2.String(start),
			End:   awsv2.String(end),
		},
	}
	if s := strings.TrimSpace(opts.Search); s != "" {
		in.SearchString = awsv2.String(s)
	}
	out, err := c.ce.GetDimensionValues(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("GetDimensionValues: %w", err)
	}
	values := make([]string, 0, len(out.DimensionValues))
	for _, dv := range out.DimensionValues {
		if v := awsv2.ToString(dv.Value); v != "" {
			values = append(values, v)
		}
	}
	sort.Strings(values)
	return &DimensionValuesResult{
		Dimension: dim,
		Start:     start,
		End:       end,
		Values:    values,
	}, nil
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// resolveDateRange fills missing start/end with sensible defaults (last
// `defaultDays` days ending today, exclusive-end). Validates format and the
// MaxDays ceiling.
func resolveDateRange(start, end string, defaultDays int) (string, string, error) {
	today := time.Now().UTC()
	if end == "" {
		end = today.Format("2006-01-02")
	}
	if start == "" {
		start = today.AddDate(0, 0, -defaultDays).Format("2006-01-02")
	}
	startT, err := time.Parse("2006-01-02", start)
	if err != nil {
		return "", "", fmt.Errorf("invalid start %q (want YYYY-MM-DD)", start)
	}
	endT, err := time.Parse("2006-01-02", end)
	if err != nil {
		return "", "", fmt.Errorf("invalid end %q (want YYYY-MM-DD)", end)
	}
	if !startT.Before(endT) {
		return "", "", fmt.Errorf("start (%s) must be before end (%s); Cost Explorer end is exclusive", start, end)
	}
	if endT.Sub(startT).Hours()/24 > float64(MaxDays) {
		return "", "", fmt.Errorf("time range exceeds %d days — split into smaller chunks to limit Cost Explorer API spend", MaxDays)
	}
	return start, end, nil
}

// parseAmount safely parses a Cost Explorer amount string, returning 0 on
// any error. Cost Explorer returns amounts as decimal strings ("1234.56").
func parseAmount(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// toForecastMetric translates a GetCostAndUsage-style metric
// ("AmortizedCost") into the GetCostForecast enum value
// ("AMORTIZED_COST"). If callers already pass the UPPER_SNAKE form it is
// accepted verbatim. Cost Explorer unfortunately accepts CamelCase for
// GetCostAndUsage but strictly UPPER_SNAKE for GetCostForecast, so we
// normalize here using the AWS SDK's typed constants.
func toForecastMetric(m string) (cetypes.Metric, error) {
	switch strings.TrimSpace(m) {
	case "AmortizedCost", string(cetypes.MetricAmortizedCost):
		return cetypes.MetricAmortizedCost, nil
	case "UnblendedCost", string(cetypes.MetricUnblendedCost):
		return cetypes.MetricUnblendedCost, nil
	case "BlendedCost", string(cetypes.MetricBlendedCost):
		return cetypes.MetricBlendedCost, nil
	case "NetAmortizedCost", string(cetypes.MetricNetAmortizedCost):
		return cetypes.MetricNetAmortizedCost, nil
	case "NetUnblendedCost", string(cetypes.MetricNetUnblendedCost):
		return cetypes.MetricNetUnblendedCost, nil
	case "UsageQuantity", string(cetypes.MetricUsageQuantity):
		return cetypes.MetricUsageQuantity, nil
	case "NormalizedUsageAmount", string(cetypes.MetricNormalizedUsageAmount):
		return cetypes.MetricNormalizedUsageAmount, nil
	default:
		return "", fmt.Errorf("invalid forecast metric %q (want AmortizedCost, UnblendedCost, BlendedCost, NetAmortizedCost, NetUnblendedCost, UsageQuantity, or NormalizedUsageAmount)", m)
	}
}
