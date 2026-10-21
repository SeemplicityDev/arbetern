package grafana

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultLogLines  = 30
	maxLogLines      = 200
	defaultSpanLines = 30
	maxSpanLines     = 200
	maxFieldKeys     = 8
	maxSeriesRows    = 20000
)

var (
	logMaps    = []string{"LogAttributes", "ResourceAttributes"}
	spanMaps   = []string{"SpanAttributes", "ResourceAttributes"}
	metricMaps = []string{"Attributes", "ResourceAttributes"}
	// metricKinds name the OpenTelemetry ClickHouse exporter's per-type metric tables.
	metricKinds = []string{"gauge", "sum", "histogram", "exponential_histogram", "summary"}
)

// otelSource locates one signal: tables from the datasource settings, columns from the exporter defaults.
type otelSource struct {
	database string
	table    string
	timeCol  string
	levelCol string
	msgCol   string
}

func logsSource(ds Datasource) otelSource {
	cfg, _ := ds.JSONData["logs"].(map[string]any)
	return otelSource{
		database: firstNonEmpty(str(cfg, "defaultDatabase"), str(ds.JSONData, "defaultDatabase"), "default"),
		table:    firstNonEmpty(str(cfg, "defaultTable"), "otel_logs"),
		timeCol:  firstNonEmpty(str(cfg, "timeColumn"), "Timestamp"),
		levelCol: firstNonEmpty(str(cfg, "levelColumn"), "SeverityText"),
		msgCol:   firstNonEmpty(str(cfg, "messageColumn"), "Body"),
	}
}

func tracesSource(ds Datasource) otelSource {
	cfg, _ := ds.JSONData["traces"].(map[string]any)
	return otelSource{
		database: firstNonEmpty(str(cfg, "defaultDatabase"), str(ds.JSONData, "defaultDatabase"), "default"),
		table:    firstNonEmpty(str(cfg, "defaultTable"), "otel_traces"),
		timeCol:  "Timestamp",
	}
}

func metricsDatabase(ds Datasource) string {
	return logsSource(ds).database
}

// attrValue reads a key from the first map that holds it.
func attrValue(maps []string, key string) string {
	k := chString(key)
	expr := maps[len(maps)-1] + "[" + k + "]"
	for i := len(maps) - 2; i >= 0; i-- {
		expr = fmt.Sprintf("if(mapContains(%s, %s), %s[%s], %s)", maps[i], k, maps[i], k, expr)
	}
	return expr
}

func attrMatch(maps []string, key, value string) string {
	k := chString(key)
	parts := make([]string, len(maps))
	for i, m := range maps {
		if strings.Contains(value, "*") {
			pattern := strings.ReplaceAll(strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value), "*", "%")
			parts[i] = fmt.Sprintf("%s[%s] LIKE %s", m, k, chString(pattern))
		} else {
			parts[i] = fmt.Sprintf("%s[%s] = %s", m, k, chString(value))
		}
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

func attrConds(maps []string, attrs map[string]string) []string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		if strings.TrimSpace(k) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = attrMatch(maps, strings.TrimSpace(k), attrs[k])
	}
	return out
}

func cleanList(vals []string) []string {
	var out []string
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// LogSearch narrows a log search.
type LogSearch struct {
	Datasources []string
	Start       string
	End         string
	Services    []string
	Severities  []string
	Text        []string
	ExcludeText []string
	Attributes  map[string]string
	TraceID     string
	Fields      []string
	CountBy     string
	Limit       int
}

// LogSearchResult is the outcome of SearchLogs.
type LogSearchResult struct {
	From    time.Time
	To      time.Time
	Search  LogSearch
	CountBy []string
	Runs    []DatasourceRun
}

// SearchLogs returns the newest matching lines, the total and a breakdown per datasource.
func (c *Client) SearchLogs(ctx context.Context, s LogSearch) (*LogSearchResult, error) {
	targets, err := c.clickHouseTargets(ctx, s.Datasources)
	if err != nil {
		return nil, err
	}
	from, to, err := TimeRange(s.Start, s.End, time.Now())
	if err != nil {
		return nil, err
	}
	s.Limit = clamp(s.Limit, 1, maxLogLines, defaultLogLines)
	s.Fields = cleanList(s.Fields)
	if len(s.Fields) > maxFieldKeys {
		s.Fields = s.Fields[:maxFieldKeys]
	}
	_, countBy, _ := logGroups(s.CountBy, "")
	res := &LogSearchResult{From: from, To: to, Search: s, CountBy: countBy}
	res.Runs = c.fanOut(targets, func(ds Datasource) (map[string]SQLResult, error) {
		src := logsSource(ds)
		table := chTable(src.database, src.table)
		level, body := chIdent(src.levelCol), chIdent(src.msgCol)
		where := logWhere(src, s)

		cols := []string{chIdent(src.timeCol) + " AS ts", "ServiceName AS service", level + " AS level", "substring(" + body + ", 1, 2000) AS body", "TraceId AS trace_id"}
		for _, f := range s.Fields {
			cols = append(cols, attrValue(logMaps, f)+" AS "+chIdent(f))
		}
		lines := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s DESC LIMIT %d",
			strings.Join(cols, ", "), table, where, chIdent(src.timeCol), s.Limit)

		groups, aliases, limit := logGroups(s.CountBy, level)
		counts := fmt.Sprintf("SELECT %s, count() AS n FROM %s WHERE %s GROUP BY %s ORDER BY n DESC LIMIT %d",
			strings.Join(groups, ", "), table, where, strings.Join(aliases, ", "), limit)
		total := fmt.Sprintf("SELECT count() AS n FROM %s WHERE %s", table, where)
		return c.runSQL(ctx, ds, from, to, sqlInterval(from, to, ""), []namedSQL{
			{ref: "lines", sql: lines}, {ref: "counts", sql: counts}, {ref: "total", sql: total},
		})
	})
	return res, nil
}

func logWhere(src otelSource, s LogSearch) string {
	level, body := chIdent(src.levelCol), chIdent(src.msgCol)
	conds := []string{"$__timeFilter(" + chIdent(src.timeCol) + ")"}
	if svc := cleanList(s.Services); len(svc) > 0 {
		conds = append(conds, "ServiceName IN ("+chList(svc)+")")
	}
	if sev := cleanList(s.Severities); len(sev) > 0 {
		parts := make([]string, len(sev))
		for i, v := range sev {
			parts[i] = "startsWith(lower(" + level + "), " + chString(strings.ToLower(v)) + ")"
		}
		conds = append(conds, "("+strings.Join(parts, " OR ")+")")
	}
	for _, t := range cleanList(s.Text) {
		conds = append(conds, "positionCaseInsensitive("+body+", "+chString(t)+") > 0")
	}
	for _, t := range cleanList(s.ExcludeText) {
		conds = append(conds, "positionCaseInsensitive("+body+", "+chString(t)+") = 0")
	}
	if id := strings.TrimSpace(s.TraceID); id != "" {
		conds = append(conds, "TraceId = "+chString(id))
	}
	conds = append(conds, attrConds(logMaps, s.Attributes)...)
	return strings.Join(conds, " AND ")
}

func logGroups(countBy, level string) ([]string, []string, int) {
	switch strings.ToLower(strings.TrimSpace(countBy)) {
	case "":
		return []string{"ServiceName AS service", level + " AS level"}, []string{"service", "level"}, 25
	case "service":
		return []string{"ServiceName AS service"}, []string{"service"}, 50
	case "severity", "level":
		return []string{level + " AS level"}, []string{"level"}, 50
	case "attribute_keys":
		return []string{"arrayJoin(arrayConcat(mapKeys(LogAttributes), mapKeys(ResourceAttributes))) AS attribute"}, []string{"attribute"}, 80
	}
	key := strings.TrimSpace(countBy)
	return []string{attrValue(logMaps, key) + " AS " + chIdent(key)}, []string{chIdent(key)}, 50
}

// MetricQuery lists metrics (no Metric) or returns one metric as series.
type MetricQuery struct {
	Datasources []string
	Start       string
	End         string
	Metric      string
	Search      string
	Services    []string
	Attributes  map[string]string
	GroupBy     []string
	Aggregation string
	Interval    string
}

// MetricResult holds per datasource one listing per metric kind, or the series under "series".
type MetricResult struct {
	From     time.Time
	To       time.Time
	Interval time.Duration
	Query    MetricQuery
	Notes    map[string]string
	Runs     []DatasourceRun
}

// QueryMetrics lists the metrics in range, or aggregates one metric per interval.
func (c *Client) QueryMetrics(ctx context.Context, q MetricQuery) (*MetricResult, error) {
	targets, err := c.clickHouseTargets(ctx, q.Datasources)
	if err != nil {
		return nil, err
	}
	from, to, err := TimeRange(q.Start, q.End, time.Now())
	if err != nil {
		return nil, err
	}
	interval := sqlInterval(from, to, q.Interval)
	q.Metric = strings.TrimSpace(q.Metric)
	res := &MetricResult{From: from, To: to, Interval: interval, Query: q, Notes: map[string]string{}}
	var mu sync.Mutex
	res.Runs = c.fanOut(targets, func(ds Datasource) (map[string]SQLResult, error) {
		db := metricsDatabase(ds)
		if q.Metric == "" {
			var queries []namedSQL
			for _, kind := range metricKinds {
				conds := metricConds(q)
				if s := strings.TrimSpace(q.Search); s != "" {
					conds = append(conds, "positionCaseInsensitive(MetricName, "+chString(s)+") > 0")
				}
				queries = append(queries, namedSQL{ref: kind, sql: fmt.Sprintf(
					"SELECT MetricName AS metric, any(MetricUnit) AS unit, any(MetricDescription) AS description, uniqExact(ServiceName) AS services, count() AS points FROM %s WHERE %s GROUP BY metric ORDER BY metric LIMIT 500",
					chTable(db, "otel_metrics_"+kind), strings.Join(conds, " AND "))})
			}
			return c.runSQL(ctx, ds, from, to, interval, queries)
		}

		probe := func(kind, extra string) namedSQL {
			return namedSQL{ref: kind, sql: fmt.Sprintf("SELECT count() AS n, any(MetricUnit) AS unit%s FROM %s WHERE MetricName = %s AND $__timeFilter(TimeUnix)",
				extra, chTable(db, "otel_metrics_"+kind), chString(q.Metric))}
		}
		found, err := c.runSQL(ctx, ds, from, to, interval, []namedSQL{
			probe("gauge", ""),
			probe("sum", ", any(AggregationTemporality) AS temporality, any(IsMonotonic) AS monotonic"),
			probe("histogram", ""),
		})
		if err != nil {
			return nil, err
		}
		kind, unit, temporality, monotonic := "", "", 0, false
		for _, k := range []string{"gauge", "sum", "histogram"} {
			rows := records(found[k])
			if len(rows) == 0 {
				continue
			}
			if n, _ := toFloat(rows[0]["n"]); n > 0 {
				kind, unit = k, asString(rows[0]["unit"])
				if k == "sum" {
					t, _ := toFloat(rows[0]["temporality"])
					temporality, monotonic = int(t), asBool(rows[0]["monotonic"])
				}
				break
			}
		}
		switch kind {
		case "":
			return map[string]SQLResult{"series": {Err: fmt.Sprintf("no %s datapoints in this range (checked gauge, sum and histogram tables); list metric names to find the right one", q.Metric)}}, nil
		case "histogram":
			return map[string]SQLResult{"series": {Err: "this metric is a histogram; query its Count, Sum and BucketCounts columns with grafana_query_sql"}}, nil
		}
		sql, note := metricSeriesSQL(chTable(db, "otel_metrics_"+kind), kind, temporality, monotonic, q)
		if unit != "" {
			note += " · unit " + unit
		}
		mu.Lock()
		res.Notes[ds.UID] = note
		mu.Unlock()
		out, err := c.runSQL(ctx, ds, from, to, interval, []namedSQL{{ref: "series", sql: sql}})
		return out, err
	})
	return res, nil
}

func metricConds(q MetricQuery) []string {
	conds := []string{"$__timeFilter(TimeUnix)"}
	if svc := cleanList(q.Services); len(svc) > 0 {
		conds = append(conds, "ServiceName IN ("+chList(svc)+")")
	}
	return append(conds, attrConds(metricMaps, q.Attributes)...)
}

// A cumulative counter's increase is taken per series before summing, so restarts and series do not mix.
func metricSeriesSQL(table, kind string, temporality int, monotonic bool, q MetricQuery) (string, string) {
	var groups, aliases []string
	for _, g := range cleanList(q.GroupBy) {
		if strings.EqualFold(g, "service") {
			groups, aliases = append(groups, "ServiceName AS service"), append(aliases, "service")
			continue
		}
		groups, aliases = append(groups, attrValue(metricMaps, g)+" AS "+chIdent(g)), append(aliases, chIdent(g))
	}
	selGroups, byGroups := "", ""
	if len(groups) > 0 {
		selGroups = ", " + strings.Join(groups, ", ")
		byGroups = ", " + strings.Join(aliases, ", ")
	}
	where := "MetricName = " + chString(q.Metric) + " AND " + strings.Join(metricConds(q), " AND ")
	cumulative := kind == "sum" && temporality == 2

	agg := strings.ToLower(strings.TrimSpace(q.Aggregation))
	if agg == "" {
		agg = "avg"
		if kind == "sum" && (!cumulative || monotonic) {
			agg = "increase"
		}
	}
	note := kind
	if kind == "sum" {
		note = map[bool]string{true: "cumulative counter", false: "delta counter"}[cumulative]
	}
	note += " · " + agg

	if agg == "increase" || agg == "rate" {
		per := ""
		if agg == "rate" {
			per = " / $__interval_s"
		}
		if !cumulative {
			return fmt.Sprintf("SELECT $__timeInterval(TimeUnix) AS t%s, sum(Value)%s AS value FROM %s WHERE %s GROUP BY t%s ORDER BY t LIMIT %d",
				selGroups, per, table, where, byGroups, maxSeriesRows), note
		}
		return fmt.Sprintf("SELECT t%s, sum(inc)%s AS value FROM (SELECT $__timeInterval(TimeUnix) AS t%s, deltaSumTimestamp(Value, TimeUnix) AS inc FROM %s WHERE %s GROUP BY t%s, cityHash64(ServiceName, Attributes, ResourceAttributes)) GROUP BY t%s ORDER BY t LIMIT %d",
			byGroups, per, selGroups, table, where, byGroups, byGroups, maxSeriesRows), note
	}
	expr := map[string]string{
		"avg": "avg(Value)", "min": "min(Value)", "max": "max(Value)", "sum": "sum(Value)",
		"last": "argMax(Value, TimeUnix)", "count": "count()",
	}[agg]
	if expr == "" {
		expr, note = "avg(Value)", kind+" · avg"
	}
	return fmt.Sprintf("SELECT $__timeInterval(TimeUnix) AS t%s, %s AS value FROM %s WHERE %s GROUP BY t%s ORDER BY t LIMIT %d",
		selGroups, expr, table, where, byGroups, maxSeriesRows), note
}

// TraceSearch finds one trace by id, or spans matching filters.
type TraceSearch struct {
	Datasources   []string
	Start         string
	End           string
	TraceID       string
	Services      []string
	SpanName      string
	MinDurationMs float64
	Status        string
	Attributes    map[string]string
	Order         string
	Limit         int
}

// TraceResult is the outcome of SearchTraces.
type TraceResult struct {
	From   time.Time
	To     time.Time
	Search TraceSearch
	Runs   []DatasourceRun
}

// SearchTraces returns one trace's spans, or matching spans with a per-operation summary.
func (c *Client) SearchTraces(ctx context.Context, s TraceSearch) (*TraceResult, error) {
	targets, err := c.clickHouseTargets(ctx, s.Datasources)
	if err != nil {
		return nil, err
	}
	s.TraceID = strings.TrimSpace(s.TraceID)
	if s.TraceID != "" && strings.TrimSpace(s.Start) == "" {
		s.Start = "now-24h"
	}
	from, to, err := TimeRange(s.Start, s.End, time.Now())
	if err != nil {
		return nil, err
	}
	s.Limit = clamp(s.Limit, 1, maxSpanLines, defaultSpanLines)
	res := &TraceResult{From: from, To: to, Search: s}
	res.Runs = c.fanOut(targets, func(ds Datasource) (map[string]SQLResult, error) {
		src := tracesSource(ds)
		table := chTable(src.database, src.table)
		if s.TraceID != "" {
			sql := fmt.Sprintf("SELECT Timestamp AS ts, ServiceName AS service, SpanName AS span, SpanKind AS kind, round(Duration / 1e6, 3) AS duration_ms, StatusCode AS status, StatusMessage AS status_message, SpanId AS span_id, ParentSpanId AS parent_span_id FROM %s WHERE TraceId = %s AND $__timeFilter(Timestamp) ORDER BY Timestamp LIMIT 1000",
				table, chString(s.TraceID))
			return c.runSQL(ctx, ds, from, to, time.Minute, []namedSQL{{ref: "trace", sql: sql}})
		}
		where := traceWhere(s)
		order := "Duration DESC"
		if strings.EqualFold(s.Order, "latest") {
			order = "Timestamp DESC"
		}
		spans := fmt.Sprintf("SELECT Timestamp AS ts, TraceId AS trace_id, ServiceName AS service, SpanName AS span, round(Duration / 1e6, 3) AS duration_ms, StatusCode AS status FROM %s WHERE %s ORDER BY %s LIMIT %d",
			table, where, order, s.Limit)
		summary := fmt.Sprintf("SELECT ServiceName AS service, SpanName AS span, count() AS spans, countIf(lower(StatusCode) IN ('error', 'status_code_error')) AS errors, round(quantile(0.5)(Duration) / 1e6, 2) AS p50_ms, round(quantile(0.95)(Duration) / 1e6, 2) AS p95_ms, round(max(Duration) / 1e6, 2) AS max_ms FROM %s WHERE %s GROUP BY service, span ORDER BY spans DESC LIMIT 25",
			table, where)
		return c.runSQL(ctx, ds, from, to, time.Minute, []namedSQL{{ref: "spans", sql: spans}, {ref: "summary", sql: summary}})
	})
	return res, nil
}

func traceWhere(s TraceSearch) string {
	conds := []string{"$__timeFilter(Timestamp)"}
	if svc := cleanList(s.Services); len(svc) > 0 {
		conds = append(conds, "ServiceName IN ("+chList(svc)+")")
	}
	if name := strings.TrimSpace(s.SpanName); name != "" {
		conds = append(conds, "positionCaseInsensitive(SpanName, "+chString(name)+") > 0")
	}
	if s.MinDurationMs > 0 {
		conds = append(conds, "Duration >= "+strconv.FormatInt(int64(s.MinDurationMs*1e6), 10))
	}
	switch strings.ToLower(strings.TrimSpace(s.Status)) {
	case "error":
		conds = append(conds, "lower(StatusCode) IN ('error', 'status_code_error')")
	case "ok":
		conds = append(conds, "lower(StatusCode) IN ('ok', 'status_code_ok')")
	case "unset":
		conds = append(conds, "lower(StatusCode) IN ('unset', 'status_code_unset', '')")
	}
	conds = append(conds, attrConds(spanMaps, s.Attributes)...)
	return strings.Join(conds, " AND ")
}
