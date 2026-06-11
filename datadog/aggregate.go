package datadog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// --------------------------------------------------------------------------
// Logs Aggregate API (v2) — server-side aggregations & percentiles
//
// Unlike SearchLogs (which returns raw log entries capped at a small page
// limit), the Logs Aggregate API computes counts, cardinalities, and
// percentiles server-side over the FULL matching dataset — no sampling, no
// row cap. This is what makes per-endpoint p95/p99 trustworthy on
// high-traffic endpoints.
//
// Requires the measured attribute (e.g. @time_took) to be registered as a
// "measure" facet in Datadog Logs config, and any group_by attribute
// (e.g. @endpoint_name, @customer_schema) to be a facet.
// --------------------------------------------------------------------------

// AggregateCompute is one server-side computation. Aggregation is one of
// count, cardinality, avg, sum, min, max, median, pc75, pc90, pc95, pc98, pc99.
// Metric is the measured attribute (required for everything except count;
// for cardinality it is the facet whose distinct values are counted).
type AggregateCompute struct {
	Aggregation string `json:"aggregation"`
	Metric      string `json:"metric,omitempty"`
	Type        string `json:"type,omitempty"` // "total" (default) or "timeseries"
}

// AggregateSort orders the buckets of a group_by by one of its computes.
type AggregateSort struct {
	Aggregation string `json:"aggregation,omitempty"`
	Metric      string `json:"metric,omitempty"`
	Order       string `json:"order,omitempty"` // "asc" | "desc"
	Type        string `json:"type,omitempty"`  // "measure" | "alphabetical"
}

// AggregateGroupBy splits results by a facet.
type AggregateGroupBy struct {
	Facet string         `json:"facet"`
	Limit int            `json:"limit,omitempty"`
	Sort  *AggregateSort `json:"sort,omitempty"`
}

type aggregateRequest struct {
	Compute []AggregateCompute     `json:"compute"`
	Filter  map[string]interface{} `json:"filter"`
	GroupBy []AggregateGroupBy     `json:"group_by,omitempty"`
}

// AggregateBucket is one group of results. By holds the facet value(s);
// Computes maps c0, c1, … (in the order of the request's compute slice) to
// their numeric results.
type AggregateBucket struct {
	By       map[string]interface{} `json:"by"`
	Computes map[string]float64     `json:"computes"`
}

// AggregateResponse is the Logs Aggregate API response envelope.
type AggregateResponse struct {
	Data struct {
		Buckets []AggregateBucket `json:"buckets"`
	} `json:"data"`
}

// LogsAggregate runs a server-side aggregation against the Logs Aggregate API.
// from/to accept ISO-8601, unix-ms, or Datadog date-math ("now-14d"); they
// default to the last 14 days.
func (c *Client) LogsAggregate(ctx context.Context, query, from, to string, compute []AggregateCompute, groupBy []AggregateGroupBy) (*AggregateResponse, error) {
	if from == "" {
		from = time.Now().Add(-14 * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	if to == "" {
		to = time.Now().UTC().Format(time.RFC3339)
	}
	body := aggregateRequest{
		Compute: compute,
		Filter: map[string]interface{}{
			"query":   query,
			"from":    from,
			"to":      to,
			"indexes": []string{"*"},
		},
		GroupBy: groupBy,
	}
	var resp AggregateResponse
	if err := c.post(ctx, "/api/v2/logs/analytics/aggregate", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// percentilePreset is the fixed set of computes used by AggregatePercentiles:
// count, then p50/p95/p99 of the measure. The c-index order MUST match.
func percentilePreset(measure string) []AggregateCompute {
	return []AggregateCompute{
		{Aggregation: "count"},                   // c0
		{Aggregation: "median", Metric: measure}, // c1 — p50
		{Aggregation: "pc95", Metric: measure},   // c2
		{Aggregation: "pc99", Metric: measure},   // c3
	}
}

// AggregatePercentiles is the high-level helper behind the datadog_logs_aggregate
// tool: for each value of groupByFacet, it returns count + p50/p95/p99 of the
// given measure, sorted by p95 descending. Runs on the requested site(s) and
// returns a Slack/markdown-friendly table per site.
func (mc *MultiClient) AggregatePercentiles(ctx context.Context, site, query, from, to, groupByFacet, measure string) (string, error) {
	if groupByFacet == "" {
		groupByFacet = "@endpoint_name"
	}
	if measure == "" {
		measure = "@time_took"
	}
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	compute := percentilePreset(measure)
	groupBy := []AggregateGroupBy{{
		Facet: groupByFacet,
		Limit: 100,
		Sort:  &AggregateSort{Aggregation: "pc95", Metric: measure, Order: "desc", Type: "measure"},
	}}
	multi := len(cs) > 1
	var parts []string
	var lastErr error
	for _, c := range cs {
		resp, err := c.LogsAggregate(ctx, query, from, to, compute, groupBy)
		if err != nil {
			lastErr = err
			if multi {
				parts = append(parts, fmt.Sprintf("*[%s]* Error: %v", c.SiteLabel(), err))
			}
			continue
		}
		parts = append(parts, FormatAggregatePercentiles(resp, c.SiteLabel(), groupByFacet, measure))
	}
	if len(parts) == 0 && lastErr != nil {
		return "", lastErr
	}
	return strings.Join(parts, "\n\n"), nil
}

// PercentileRow is one group's percentile result: the facet value plus
// count/p50/p95/p99 of the measure.
type PercentileRow struct {
	Key   string
	Count float64
	P50   float64
	P95   float64
	P99   float64
}

// PercentileResult is the structured, per-site result of AggregatePercentilesData.
type PercentileResult struct {
	Site    string
	GroupBy string // facet name without leading "@" (e.g. "endpoint_name")
	Measure string // measure facet without leading "@" (e.g. "time_took")
	Rows    []PercentileRow
}

// AggregatePercentilesData is the structured sibling of AggregatePercentiles:
// it returns one PercentileResult per queried site (rows sorted by p95 desc),
// so callers can render the data as CSV or other formats without re-parsing
// the Slack table. Sites that error are skipped; if every site errors the
// last error is returned.
func (mc *MultiClient) AggregatePercentilesData(ctx context.Context, site, query, from, to, groupByFacet, measure string) ([]PercentileResult, error) {
	if groupByFacet == "" {
		groupByFacet = "@endpoint_name"
	}
	if measure == "" {
		measure = "@time_took"
	}
	cs := mc.clients(site)
	if len(cs) == 0 {
		return nil, fmt.Errorf("no Datadog client configured for site %q", site)
	}
	compute := percentilePreset(measure)
	groupBy := []AggregateGroupBy{{
		Facet: groupByFacet,
		Limit: 100,
		Sort:  &AggregateSort{Aggregation: "pc95", Metric: measure, Order: "desc", Type: "measure"},
	}}
	facet := strings.TrimPrefix(groupByFacet, "@")
	meas := strings.TrimPrefix(measure, "@")
	var results []PercentileResult
	var lastErr error
	for _, c := range cs {
		resp, err := c.LogsAggregate(ctx, query, from, to, compute, groupBy)
		if err != nil {
			lastErr = err
			continue
		}
		res := PercentileResult{Site: c.SiteLabel(), GroupBy: facet, Measure: meas}
		if resp != nil {
			buckets := resp.Data.Buckets
			sort.SliceStable(buckets, func(i, j int) bool { return buckets[i].Computes["c2"] > buckets[j].Computes["c2"] })
			for _, b := range buckets {
				name := "—"
				if v, ok := b.By[groupByFacet]; ok && v != nil {
					name = fmt.Sprintf("%v", v)
				}
				res.Rows = append(res.Rows, PercentileRow{
					Key:   name,
					Count: b.Computes["c0"],
					P50:   b.Computes["c1"],
					P95:   b.Computes["c2"],
					P99:   b.Computes["c3"],
				})
			}
		}
		results = append(results, res)
	}
	if len(results) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return results, nil
}

// FormatAggregatePercentiles renders a percentile-preset response as a table.
func FormatAggregatePercentiles(resp *AggregateResponse, siteLabel, groupByFacet, measure string) string {
	facet := strings.TrimPrefix(groupByFacet, "@")
	var sb strings.Builder
	fmt.Fprintf(&sb, "*[%s]* %s — p50/p95/p99 of `%s` (sorted by p95 desc)\n", siteLabel, facet, measure)
	if resp == nil || len(resp.Data.Buckets) == 0 {
		sb.WriteString("_No matching samples in the window._")
		return sb.String()
	}
	rows := resp.Data.Buckets
	// Defensive: re-sort by p95 (c2) desc in case the API limit clipped before sorting.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Computes["c2"] > rows[j].Computes["c2"] })
	fmt.Fprintf(&sb, "%-32s %10s %10s %10s %10s\n", facet, "count", "p50", "p95", "p99")
	for _, b := range rows {
		name := "—"
		if v, ok := b.By[groupByFacet]; ok && v != nil {
			name = fmt.Sprintf("%v", v)
		}
		if len(name) > 32 {
			name = name[:31] + "…"
		}
		fmt.Fprintf(&sb, "%-32s %10.0f %10.3f %10.3f %10.3f\n",
			name, b.Computes["c0"], b.Computes["c1"], b.Computes["c2"], b.Computes["c3"])
	}
	return sb.String()
}
