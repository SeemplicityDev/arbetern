package datadog

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/series"
	"github.com/justmike1/arbetern/internal/text"
)

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

// AggregateCompute is one server-side computation. Aggregation is one of
// count, cardinality, avg, sum, min, max, median, pc75, pc90, pc95, pc98, pc99.
// Metric is the measured attribute (required for everything except count;
// for cardinality it is the facet whose distinct values are counted).
type AggregateCompute struct {
	Aggregation string `json:"aggregation"`
	Metric      string `json:"metric,omitempty"`
	Type        string `json:"type,omitempty"` // "total" (default) or "timeseries"
	Interval    string `json:"interval,omitempty"`
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
	var resp AggregateResponse
	if err := c.aggregate(ctx, query, from, to, compute, groupBy, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) aggregate(ctx context.Context, query, from, to string, compute []AggregateCompute, groupBy []AggregateGroupBy, out any) error {
	from, to = logsWindow(from, to)
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
	return c.post(ctx, "/api/v2/logs/analytics/aggregate", body, out)
}

func logsWindow(from, to string) (string, string) {
	if from == "" {
		from = time.Now().Add(-14 * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	if to == "" {
		to = time.Now().UTC().Format(time.RFC3339)
	}
	return NormalizeLogsTime(from), NormalizeLogsTime(to)
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

// facetLimits[n-1] holds the per-facet bucket limits of an n-facet group_by.
// Datadog rejects a group_by whose limits multiply past 10,000, so outer facets
// keep more buckets than inner ones.
var facetLimits = [][]int{
	{100},
	{100, 100},
	{50, 20, 10},
	{20, 10, 10, 5},
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
	for i, f := range facets {
		gb = append(gb, AggregateGroupBy{
			Facet: f,
			Limit: facetLimits[len(facets)-1][i],
			Sort:  sortSpec,
		})
	}
	return gb
}

func clipNote(buckets []AggregateBucket, facets []string) string {
	if len(facets) == 0 {
		return ""
	}
	limits := facetLimits[len(facets)-1]
	var hit []string
	for i, f := range facets {
		children := map[string]map[string]bool{}
		for _, b := range buckets {
			parent := bucketKey(b.By, facets[:i])
			if children[parent] == nil {
				children[parent] = map[string]bool{}
			}
			children[parent][fmt.Sprint(b.By[f])] = true
		}
		for _, values := range children {
			if len(values) >= limits[i] {
				hit = append(hit, fmt.Sprintf("%s %d", strings.TrimPrefix(f, "@"), limits[i]))
				break
			}
		}
	}
	if len(hit) == 0 {
		return ""
	}
	return fmt.Sprintf("\n_Truncated at the per-facet limit (%s): values past it are missing, so sums over this table undercount._", strings.Join(hit, ", "))
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

// AggregatePercentilesReport is the high-level helper behind the
// datadog_logs_aggregate tool. For each bucket of the group-by facet(s) it
// returns the event count and, when a measure is given, p50/p95/p99 of that
// measure (sorted by p95 desc; by count desc when no measure). Passing more
// than one facet produces a composite breakdown (one row per facet-value
// combination); passing none returns a single overall bucket.
//
// It runs each site's aggregation EXACTLY ONCE, deriving both the
// Slack/markdown table and the structured per-site rows from the same response.
// Querying once (instead of once to format and again to cache) halves the
// Datadog API load per tool call — important for the smaller EU org, which is
// quick to 429 rate-limit under the report's burst of aggregate calls.
func (mc *MultiClient) AggregatePercentilesReport(ctx context.Context, site, query, from, to string, groupBy []string, measure string) (string, []PercentileResult, error) {
	facets := normalizeGroupByFacets(groupBy)
	if len(facets) > len(facetLimits) {
		return "", nil, fmt.Errorf("group_by takes at most %d facets, got %d", len(facetLimits), len(facets))
	}
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", nil, fmt.Errorf("no Datadog client configured for site %q", site)
	}
	compute := percentilePreset(measure)
	groupByReq := buildPercentileGroupBy(facets, measure)
	multi := len(cs) > 1
	var (
		parts   []string
		results []PercentileResult
		lastErr error
	)
	for _, c := range cs {
		resp, err := c.LogsAggregate(ctx, query, from, to, compute, groupByReq)
		if err != nil {
			lastErr = err
			if multi {
				parts = append(parts, fmt.Sprintf("*[%s]* Error: %v", c.SiteLabel(), err))
			}
			continue
		}
		parts = append(parts, FormatAggregatePercentiles(resp, c.SiteLabel(), facets, measure)+clipNote(resp.Data.Buckets, facets))
		results = append(results, percentileResultFromResponse(resp, c.SiteLabel(), facets, measure))
	}
	if len(parts) == 0 && lastErr != nil {
		return "", nil, lastErr
	}
	return strings.Join(parts, "\n\n"), results, nil
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

// percentileResultFromResponse converts one site's aggregate response into a
// PercentileResult with rows sorted by p95 desc (by count desc when no measure
// is given), matching the ordering of the rendered table.
func percentileResultFromResponse(resp *AggregateResponse, siteLabel string, facets []string, measure string) PercentileResult {
	res := PercentileResult{
		Site:    siteLabel,
		GroupBy: joinFacetNames(facets),
		Measure: strings.TrimPrefix(measure, "@"),
	}
	if resp == nil {
		return res
	}
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
	return res
}

// FormatAggregatePercentiles renders an aggregate response as a table. facets
// may hold one or more group-by facets; for multiple facets each row's key is
// the composite "value1 | value2". With no measure it renders a count-only
// table sorted by count.
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
	t := text.Table{
		Headers: []string{label, "count"},
		Align:   []text.Align{text.AlignLeft, text.AlignRight, text.AlignRight, text.AlignRight, text.AlignRight},
		NoFence: true,
	}
	if measure != "" {
		t.Headers = append(t.Headers, "p50", "p95", "p99")
	}
	for _, b := range rows {
		row := []string{bucketKey(b.By, facets), fmt.Sprintf("%.0f", b.Computes["c0"])}
		if measure != "" {
			row = append(row, fmt.Sprintf("%.3f", b.Computes["c1"]), fmt.Sprintf("%.3f", b.Computes["c2"]), fmt.Sprintf("%.3f", b.Computes["c3"]))
		}
		t.Rows = append(t.Rows, row)
	}
	sb.WriteString(t.String())
	return sb.String()
}

const (
	maxCountGroups  = 10
	maxCountBuckets = 1500
)

type countSeriesResponse struct {
	Data struct {
		Buckets []struct {
			By       map[string]interface{} `json:"by"`
			Computes map[string][]struct {
				Time  any     `json:"time"`
				Value float64 `json:"value"`
			} `json:"computes"`
		} `json:"buckets"`
	} `json:"data"`
}

// LogCountsOverTime counts matching logs per interval for the top groups of at most one facet, with each group's level shifts.
func (mc *MultiClient) LogCountsOverTime(ctx context.Context, site, query, from, to string, groupBy []string, interval string) (string, error) {
	facets := normalizeGroupByFacets(groupBy)
	if len(facets) > 1 {
		return "", fmt.Errorf("interval takes at most one group_by facet, got %d", len(facets))
	}
	interval = strings.TrimSpace(interval)
	step, err := parseRelativeDuration(interval)
	if err != nil || step <= 0 {
		return "", fmt.Errorf("interval must be a duration such as 1m, 5m, 1h or 1d, got %q", interval)
	}
	from, to = logsWindow(from, to)
	start, startOK := logsInstant(from)
	end, endOK := logsInstant(to)
	if startOK && endOK && start.Before(end) {
		if n := int(end.Sub(start)/step) + 1; n > maxCountBuckets {
			return "", fmt.Errorf("interval %s gives %d buckets over this window; use a larger interval (at most %d buckets)", interval, n, maxCountBuckets)
		}
	} else {
		start, end = time.Time{}, time.Time{}
	}
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	compute := []AggregateCompute{{Aggregation: "count", Type: "timeseries", Interval: interval}}
	groupByReq := buildPercentileGroupBy(facets, "")
	for i := range groupByReq {
		groupByReq[i].Limit = maxCountGroups
	}
	multi := len(cs) > 1
	var (
		parts   []string
		lastErr error
	)
	for _, c := range cs {
		var resp countSeriesResponse
		if err := c.aggregate(ctx, query, from, to, compute, groupByReq, &resp); err != nil {
			lastErr = err
			if multi {
				parts = append(parts, fmt.Sprintf("*[%s]* Error: %v", c.SiteLabel(), err))
			}
			continue
		}
		parts = append(parts, formatLogCounts(&resp, c.SiteLabel(), facets, step, start, end))
	}
	if len(parts) == 0 && lastErr != nil {
		return "", lastErr
	}
	return strings.Join(parts, "\n\n"), nil
}

func formatLogCounts(resp *countSeriesResponse, siteLabel string, facets []string, step time.Duration, start, end time.Time) string {
	type group struct {
		key    string
		total  float64
		points []series.Point
	}
	groups := make([]group, 0, len(resp.Data.Buckets))
	var seen []time.Time
	for _, b := range resp.Data.Buckets {
		g := group{key: bucketKey(b.By, facets)}
		for _, p := range b.Computes["c0"] {
			if t, ok := bucketTime(p.Time); ok {
				g.points = append(g.points, series.Point{At: t, Value: p.Value})
				g.total += p.Value
				seen = append(seen, t)
			}
		}
		groups = append(groups, g)
	}
	sort.Slice(seen, func(i, j int) bool { return seen[i].Before(seen[j]) })
	var gap time.Duration
	for i := 1; i < len(seen); i++ {
		if d := seen[i].Sub(seen[i-1]); d > 0 && (gap == 0 || d < gap) {
			gap = d
		}
	}
	step = max(step, gap)
	var sb strings.Builder
	fmt.Fprintf(&sb, "*[%s]* %s — log count per %s\n", siteLabel, joinFacetNames(facets), series.Duration(step))
	if len(seen) == 0 {
		sb.WriteString("_No matching logs in the window._")
		return sb.String()
	}
	if start.IsZero() {
		start, end = seen[0], seen[len(seen)-1].Add(step)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].total > groups[j].total })
	for _, g := range groups {
		slots := map[int64]float64{}
		for _, p := range g.points {
			slots[p.At.Truncate(step).Unix()] += p.Value
		}
		var pts []series.Point
		for t := start.Truncate(step); t.Before(end) && len(pts) < maxCountBuckets; t = t.Add(step) {
			pts = append(pts, series.Point{At: t, Value: slots[t.Unix()]})
		}
		fmt.Fprintf(&sb, "• %s — %s total", g.key, strconv.FormatFloat(g.total, 'f', 0, 64))
		if shape := series.Shape(pts); shape != "" {
			fmt.Fprintf(&sb, " · %s", shape)
		}
		sb.WriteString("\n")
	}
	if len(groups) >= maxCountGroups {
		fmt.Fprintf(&sb, "_Top %d groups by count only._\n", maxCountGroups)
	}
	return sb.String()
}

func logsInstant(v string) (time.Time, bool) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 1e11 {
		return time.UnixMilli(n), true
	}
	s, err := parseTimeArg(v, time.Time{})
	return time.Unix(s, 0), err == nil
}

func bucketTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		ts, err := time.Parse(time.RFC3339, t)
		return ts, err == nil
	case float64:
		return time.UnixMilli(int64(t)), true
	}
	return time.Time{}, false
}
