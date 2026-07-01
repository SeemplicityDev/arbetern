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
// Requires the measured attribute (e.g. @duration) to be registered as a
// "measure" facet in Datadog Logs config, and any group_by attribute
// (e.g. @http.method, @http.status_code) to be a facet.
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

// percentilePreset is the compute set for AggregatePercentiles: always count
// (c0). When a measure is given it also adds p50/p95/p99 of that measure
// (c1/c2/c3); with no measure it is a count-only aggregation. The c-index
// order MUST match the readers below.
func percentilePreset(measure string) []AggregateCompute {
	compute := []AggregateCompute{{Aggregation: "count"}} // c0
	if measure != "" {
		compute = append(compute,
			AggregateCompute{Aggregation: "median", Metric: measure}, // c1 — p50
			AggregateCompute{Aggregation: "pc95", Metric: measure},   // c2
			AggregateCompute{Aggregation: "pc99", Metric: measure},   // c3
		)
	}
	return compute
}

// normalizeGroupByFacets cleans a group-by specification into an ordered,
// de-duplicated slice of facets. Each element may itself be a comma-separated
// list (so callers can pass "@a,@b" or a real slice). An empty specification
// yields an empty slice, meaning "no grouping" (a single overall bucket).
func normalizeGroupByFacets(facets []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range facets {
		for _, part := range strings.Split(f, ",") {
			p := strings.TrimSpace(part)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// buildPercentileGroupBy builds the request's group_by slice: one entry per
// facet, each limited and sorted descending — by the p95 measure when a measure
// is given, otherwise by count. Multiple entries produce nested buckets whose
// `by` map carries every facet value. An empty facet slice yields no group_by
// (a single overall bucket).
func buildPercentileGroupBy(facets []string, measure string) []AggregateGroupBy {
	sortSpec := &AggregateSort{Aggregation: "count", Order: "desc", Type: "measure"}
	if measure != "" {
		sortSpec = &AggregateSort{Aggregation: "pc95", Metric: measure, Order: "desc", Type: "measure"}
	}
	gb := make([]AggregateGroupBy, 0, len(facets))
	for _, f := range facets {
		gb = append(gb, AggregateGroupBy{
			Facet: f,
			Limit: 100,
			Sort:  sortSpec,
		})
	}
	return gb
}

// sortComputeIndex is the computes key rows are ranked by: p95 (c2) when a
// measure is present, otherwise count (c0).
func sortComputeIndex(measure string) string {
	if measure == "" {
		return "c0"
	}
	return "c2"
}

// joinFacetNames renders the facet list as a stable label, stripping the
// leading "@" from each and joining multiple facets with "+" (e.g. "a+b").
// An empty facet slice renders as "all". Used for table headings and the CSV
// group_by column.
func joinFacetNames(facets []string) string {
	if len(facets) == 0 {
		return "all"
	}
	names := make([]string, len(facets))
	for i, f := range facets {
		names[i] = strings.TrimPrefix(f, "@")
	}
	return strings.Join(names, "+")
}

// bucketKey builds a display key for one bucket by concatenating its value for
// each facet in order, joined with " | ". Missing values render as "—". An
// empty facet slice (no grouping) renders as "all".
func bucketKey(by map[string]interface{}, facets []string) string {
	if len(facets) == 0 {
		return "all"
	}
	parts := make([]string, 0, len(facets))
	for _, f := range facets {
		v := "—"
		if val, ok := by[f]; ok && val != nil {
			v = fmt.Sprintf("%v", val)
		}
		parts = append(parts, v)
	}
	return strings.Join(parts, " | ")
}

// AggregatePercentiles is the high-level helper behind the datadog_logs_aggregate
// tool: for each bucket of the group-by facet(s) it returns the event count and,
// when a measure is given, p50/p95/p99 of that measure (sorted by p95 desc; by
// count desc when no measure). Passing more than one facet produces a composite
// breakdown (one row per facet-value combination); passing none returns a single
// overall bucket. Runs on the requested site(s) and returns a Slack/markdown-
// friendly table per site.
func (mc *MultiClient) AggregatePercentiles(ctx context.Context, site, query, from, to string, groupBy []string, measure string) (string, error) {
	facets := normalizeGroupByFacets(groupBy)
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	compute := percentilePreset(measure)
	groupByReq := buildPercentileGroupBy(facets, measure)
	multi := len(cs) > 1
	var parts []string
	var lastErr error
	for _, c := range cs {
		resp, err := c.LogsAggregate(ctx, query, from, to, compute, groupByReq)
		if err != nil {
			lastErr = err
			if multi {
				parts = append(parts, fmt.Sprintf("*[%s]* Error: %v", c.SiteLabel(), err))
			}
			continue
		}
		parts = append(parts, FormatAggregatePercentiles(resp, c.SiteLabel(), facets, measure))
	}
	if len(parts) == 0 && lastErr != nil {
		return "", lastErr
	}
	return strings.Join(parts, "\n\n"), nil
}

// PercentileRow is one bucket's result: the (composite) group key plus the
// event count and, when a measure was requested, p50/p95/p99 of the measure
// (0 for a count-only aggregation). For multi-facet group-bys Key holds the
// facet values joined with " | "; when ungrouped Key is "all".
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
	GroupBy string // facet name(s) without leading "@"; multiple facets joined with "+" (e.g. "a+b"), or "all" when ungrouped
	Measure string // measure facet without leading "@"
	Rows    []PercentileRow
}

// AggregatePercentilesData is the structured sibling of AggregatePercentiles:
// it returns one PercentileResult per queried site (rows sorted by p95 desc),
// so callers can render the data as CSV or other formats without re-parsing
// the Slack table. Sites that error are skipped; if every site errors the
// last error is returned.
func (mc *MultiClient) AggregatePercentilesData(ctx context.Context, site, query, from, to string, groupBy []string, measure string) ([]PercentileResult, error) {
	facets := normalizeGroupByFacets(groupBy)
	cs := mc.clients(site)
	if len(cs) == 0 {
		return nil, fmt.Errorf("no Datadog client configured for site %q", site)
	}
	compute := percentilePreset(measure)
	groupByReq := buildPercentileGroupBy(facets, measure)
	groupByName := joinFacetNames(facets)
	meas := strings.TrimPrefix(measure, "@")
	var results []PercentileResult
	var lastErr error
	for _, c := range cs {
		resp, err := c.LogsAggregate(ctx, query, from, to, compute, groupByReq)
		if err != nil {
			lastErr = err
			continue
		}
		res := PercentileResult{Site: c.SiteLabel(), GroupBy: groupByName, Measure: meas}
		if resp != nil {
			buckets := resp.Data.Buckets
			idx := sortComputeIndex(measure)
			sort.SliceStable(buckets, func(i, j int) bool { return buckets[i].Computes[idx] > buckets[j].Computes[idx] })
			for _, b := range buckets {
				res.Rows = append(res.Rows, PercentileRow{
					Key:   bucketKey(b.By, facets),
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

// FormatAggregatePercentiles renders an aggregate response as a table. facets
// may hold one or more group-by facets; for multiple facets each row's key is
// the composite "value1 | value2" and the key column is widened. With no
// measure it renders a count-only table sorted by count.
func FormatAggregatePercentiles(resp *AggregateResponse, siteLabel string, facets []string, measure string) string {
	facets = normalizeGroupByFacets(facets)
	label := joinFacetNames(facets)
	var sb strings.Builder
	if measure == "" {
		fmt.Fprintf(&sb, "*[%s]* %s — event counts (sorted desc)\n", siteLabel, label)
	} else {
		fmt.Fprintf(&sb, "*[%s]* %s — p50/p95/p99 of `%s` (sorted by p95 desc)\n", siteLabel, label, measure)
	}
	if resp == nil || len(resp.Data.Buckets) == 0 {
		sb.WriteString("_No matching samples in the window._")
		return sb.String()
	}
	rows := resp.Data.Buckets
	// Defensive: re-sort desc in case the API limit clipped before sorting.
	idx := sortComputeIndex(measure)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Computes[idx] > rows[j].Computes[idx] })
	// Widen the key column for composite (multi-facet) keys.
	keyWidth := 32
	if len(facets) > 1 {
		keyWidth = 48
	}
	if measure == "" {
		fmt.Fprintf(&sb, "%-*s %10s\n", keyWidth, label, "count")
		for _, b := range rows {
			name := bucketKey(b.By, facets)
			if len(name) > keyWidth {
				name = name[:keyWidth-1] + "…"
			}
			fmt.Fprintf(&sb, "%-*s %10.0f\n", keyWidth, name, b.Computes["c0"])
		}
		return sb.String()
	}
	fmt.Fprintf(&sb, "%-*s %10s %10s %10s %10s\n", keyWidth, label, "count", "p50", "p95", "p99")
	for _, b := range rows {
		name := bucketKey(b.By, facets)
		if len(name) > keyWidth {
			name = name[:keyWidth-1] + "…"
		}
		fmt.Fprintf(&sb, "%-*s %10.0f %10.3f %10.3f %10.3f\n",
			keyWidth, name, b.Computes["c0"], b.Computes["c1"], b.Computes["c2"], b.Computes["c3"])
	}
	return sb.String()
}
