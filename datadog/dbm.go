package datadog

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/text"
)

const (
	dbmSamplesPath = "/api/v1/logs-analytics/list?type=databasequery"
	dbmSamplesScan = 1000

	defaultDBMStatements = 10
	maxDBMStatements     = 50
	maxDBMGroupByTags    = 2
	defaultDBMGroups     = 10
	maxDBMGroups         = 30
	dbmDetailGroups      = 3
	dbmDetailLeaves      = 30
	dbmPlanNodes         = 20
	dbmTopTextChars      = 300
	dbmSampleTextChars   = 1500
	dbmPlanCondChars     = 160
)

// DBMSortMeanTime ranks statements by mean time per call instead of total time.
const DBMSortMeanTime = "mean_time"

var (
	dbmTagKeyRE    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_./-]*$`)
	dbmSignatureRE = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	dbmShownAttrs  = []string{
		"db.statement", "db.query_signature", "db.plan", "db.user", "db.application", "db.instance",
		"db.wait_event", "db.rows", "dbm_type", "duration", "timestamp", "host", "database_instance",
		"ddtags", "ddsource", "ddagentversion", "cloud_metadata",
	}
)

// DBMTopQueriesOptions selects the Postgres statements DBMTopQueries ranks.
type DBMTopQueriesOptions struct {
	Scope   string
	GroupBy []string
	Sort    string
	Limit   int
	From    string
	To      string
}

// DBMSamplesOptions selects the query samples and explain plans DBMSamples reads.
type DBMSamplesOptions struct {
	Query string
	Limit int
	From  string
	To    string
}

// DBMTopQueries ranks Postgres statements by Database Monitoring query metrics on the target site(s).
func (mc *MultiClient) DBMTopQueries(ctx context.Context, site string, opts DBMTopQueriesOptions) (string, error) {
	return mc.perSite(site, func(c *Client) (string, error) {
		res, err := c.dbmTopQueries(ctx, opts)
		if err != nil {
			return "", err
		}
		return res.format(c.SiteLabel()), nil
	})
}

// DBMSamples searches Database Monitoring query samples and explain plans on the target site(s), grouped by statement.
func (mc *MultiClient) DBMSamples(ctx context.Context, site string, opts DBMSamplesOptions) (string, error) {
	return mc.perSite(site, func(c *Client) (string, error) {
		res, err := c.dbmSamples(ctx, opts)
		if err != nil {
			return "", err
		}
		return res.format(c.SiteLabel()), nil
	})
}

func (mc *MultiClient) perSite(site string, run func(*Client) (string, error)) (string, error) {
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	var parts []string
	var errs []error
	for _, c := range cs {
		out, err := run(c)
		if err != nil {
			errs = append(errs, fmt.Errorf("[%s] %w", c.SiteLabel(), err))
			parts = append(parts, fmt.Sprintf("*[%s]* Error: %v", c.SiteLabel(), err))
			continue
		}
		parts = append(parts, out)
	}
	if len(errs) == len(cs) {
		return "", errors.Join(errs...)
	}
	return strings.Join(parts, "\n\n"), nil
}

type dbmStatement struct {
	signature string
	groups    []string
	totalNs   float64
	calls     float64
	rows      float64
	hasTotal  bool
	hasCalls  bool
	hasRows   bool
	text      string
}

type dbmTopResult struct {
	scope        string
	groupBy      []string
	sort         string
	fromMs, toMs int64
	scopeNs      float64
	hasScope     bool
	statements   []dbmStatement
	notes        []string
}

func (c *Client) dbmTopQueries(ctx context.Context, o DBMTopQueriesOptions) (*dbmTopResult, error) {
	fromMs, toMs, err := windowMs(o.From, o.To, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	groupBy, err := dbmGroupByTags(o.GroupBy)
	if err != nil {
		return nil, err
	}
	res := &dbmTopResult{scope: metricScope(o.Scope), groupBy: groupBy, sort: "total_time", fromMs: fromMs, toMs: toMs}
	if o.Sort == DBMSortMeanTime {
		res.sort = DBMSortMeanTime
	}
	limit := boundedOrDefault(o.Limit, defaultDBMStatements, maxDBMStatements)
	keys := append([]string{"query_signature"}, groupBy...)

	var ranked []scalarRow
	var unit string
	if res.sort == DBMSortMeanTime {
		ranked, _, err = c.queryScalar(ctx, fromMs, toMs, "t / c", map[string]string{
			"t": dbmMetricQuery("time", res.scope, keys),
			"c": dbmMetricQuery("count", res.scope, keys),
		}, keys, limit)
	} else {
		ranked, unit, err = c.queryScalar(ctx, fromMs, toMs, "t", map[string]string{"t": dbmMetricQuery("time", res.scope, keys)}, keys, limit)
	}
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(ranked, func(a, b scalarRow) int { return cmp.Compare(b.value, a.value) })
	ranked = ranked[:min(len(ranked), limit)]

	index := map[string]int{}
	var sigs []string
	for _, r := range ranked {
		st := dbmStatement{signature: r.tags["query_signature"], groups: tagValues(r.tags, groupBy)}
		if res.sort != DBMSortMeanTime {
			st.totalNs, st.hasTotal = r.value*nanosPer(unit), true
		}
		index[statementKey(st.signature, st.groups)] = len(res.statements)
		res.statements = append(res.statements, st)
		if dbmSignatureRE.MatchString(st.signature) && !slices.Contains(sigs, st.signature) {
			sigs = append(sigs, st.signature)
		}
	}
	if len(res.statements) == 0 {
		return res, nil
	}

	if total, unit, err := c.queryScalar(ctx, fromMs, toMs, "t", map[string]string{"t": "sum:postgresql.queries.time{" + res.scope + "}"}, nil, 0); err != nil {
		res.notes = append(res.notes, "total for the scope unavailable: "+err.Error())
	} else if len(total) > 0 {
		res.scopeNs, res.hasScope = total[0].value*nanosPer(unit), true
	}
	if len(sigs) == 0 {
		return res, nil
	}

	narrowed := signatureScope(res.scope, sigs)
	merge := func(metric string, extra []string, apply func(st *dbmStatement, r scalarRow, unit string)) {
		k := append(append([]string{"query_signature"}, extra...), groupBy...)
		rows, unit, err := c.queryScalar(ctx, fromMs, toMs, "v", map[string]string{"v": dbmMetricQuery(metric, narrowed, k)}, k, min(len(sigs)*25, 1000))
		if err != nil {
			res.notes = append(res.notes, fmt.Sprintf("postgresql.queries.%s unavailable: %v", metric, err))
			return
		}
		for _, r := range rows {
			if i, ok := index[statementKey(r.tags["query_signature"], tagValues(r.tags, groupBy))]; ok {
				apply(&res.statements[i], r, unit)
			}
		}
	}
	if res.sort == DBMSortMeanTime {
		merge("time", nil, func(st *dbmStatement, r scalarRow, unit string) {
			st.totalNs += r.value * nanosPer(unit)
			st.hasTotal = true
		})
	}
	merge("count", []string{"query"}, func(st *dbmStatement, r scalarRow, _ string) {
		st.calls += r.value
		st.hasCalls = true
		if t := r.tags["query"]; t != "N/A" && len(t) > len(st.text) {
			st.text = t
		}
	})
	merge("rows", nil, func(st *dbmStatement, r scalarRow, _ string) {
		st.rows += r.value
		st.hasRows = true
	})
	return res, nil
}

func (r *dbmTopResult) format(siteLabel string) string {
	var sb strings.Builder
	order := "total time"
	if r.sort == DBMSortMeanTime {
		order = "mean time per call"
	}
	fmt.Fprintf(&sb, "*[%s]* Postgres statements by %s · scope `%s` · %s → %s\n", siteLabel, order, r.scope, formatMs(r.fromMs), formatMs(r.toMs))
	if r.hasScope {
		fmt.Fprintf(&sb, "All statements in scope: %s of execution time\n", formatNanos(r.scopeNs))
	}
	if len(r.statements) == 0 {
		sb.WriteString("No DBM query metrics matched. Check the scope against the tags the Postgres monitors use, and that Database Monitoring runs on these hosts.\n")
		return sb.String()
	}
	for i, st := range r.statements {
		fmt.Fprintf(&sb, "\n%d. `%s`", i+1, st.signature)
		for j, tag := range r.groupBy {
			fmt.Fprintf(&sb, " · %s:%s", tag, st.groups[j])
		}
		var parts []string
		if st.hasTotal {
			p := "total " + formatNanos(st.totalNs)
			if r.hasScope && r.scopeNs > 0 {
				p += fmt.Sprintf(" (%.1f%% of scope)", 100*st.totalNs/r.scopeNs)
			}
			parts = append(parts, p)
		}
		if st.hasCalls {
			parts = append(parts, formatCount(st.calls)+" calls")
			if st.calls > 0 && st.hasTotal {
				parts = append(parts, "mean "+formatNanos(st.totalNs/st.calls))
			}
			if st.calls > 0 && st.hasRows {
				parts = append(parts, formatQuantity(st.rows/st.calls)+" rows/call")
			}
		}
		if len(parts) > 0 {
			sb.WriteString(" — " + strings.Join(parts, " · "))
		}
		sb.WriteString("\n")
		if st.text != "" {
			fmt.Fprintf(&sb, "   %s\n", sqlExcerpt(st.text, dbmTopTextChars))
		}
	}
	for _, n := range r.notes {
		fmt.Fprintf(&sb, "\nNote: %s\n", n)
	}
	sb.WriteString("\nMean is total time ÷ calls from pg_stat_statements; a statement still running is counted only once it finishes, so use datadog_dbm_samples for those. The text is the metric's query tag, cut at 200 characters; datadog_dbm_samples with @db.query_signature:<sig> returns the full statement and its explain plans.\n")
	return sb.String()
}

func dbmMetricQuery(metric, scope string, keys []string) string {
	return fmt.Sprintf("sum:postgresql.queries.%s{%s} by {%s}", metric, scope, strings.Join(keys, ","))
}

func dbmGroupByTags(in []string) ([]string, error) {
	var out []string
	for _, item := range in {
		for _, tag := range strings.Split(item, ",") {
			tag = strings.TrimSpace(tag)
			if tag == "" || tag == "query_signature" || tag == "query" || slices.Contains(out, tag) {
				continue
			}
			if !dbmTagKeyRE.MatchString(tag) {
				return nil, fmt.Errorf("invalid group_by tag %q", tag)
			}
			out = append(out, tag)
		}
	}
	if len(out) > maxDBMGroupByTags {
		return nil, fmt.Errorf("group_by takes at most %d tags", maxDBMGroupByTags)
	}
	return out, nil
}

func metricScope(scope string) string {
	s := strings.TrimSpace(scope)
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}"))
	return cmp.Or(s, "*")
}

func signatureScope(scope string, sigs []string) string {
	in := "query_signature IN (" + strings.Join(sigs, ", ") + ")"
	if scope == "*" {
		return in
	}
	// Datadog rejects the symbolic "," and "!" filters next to AND/IN, so rewrite them first.
	if !booleanScope(scope) {
		terms := strings.Split(scope, ",")
		for i, t := range terms {
			t = strings.TrimSpace(t)
			if rest, ok := strings.CutPrefix(t, "!"); ok {
				t = "NOT " + rest
			}
			terms[i] = t
		}
		scope = strings.Join(terms, " AND ")
	}
	if strings.Contains(strings.ToUpper(scope), " OR ") {
		scope = "(" + scope + ")"
	}
	return scope + " AND " + in
}

func booleanScope(s string) bool {
	u := " " + strings.ToUpper(s) + " "
	for _, op := range []string{" AND ", " OR ", " NOT ", " IN ", "("} {
		if strings.Contains(u, op) {
			return true
		}
	}
	return false
}

func statementKey(sig string, groups []string) string {
	return sig + "\x00" + strings.Join(groups, "\x00")
}

func tagValues(tags map[string]string, keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = tags[k]
	}
	return out
}

func nanosPer(unit string) float64 {
	switch unit {
	case "microsecond":
		return 1e3
	case "millisecond":
		return 1e6
	case "second":
		return 1e9
	case "minute":
		return 60e9
	case "hour":
		return 3600e9
	}
	return 1
}

type scalarColumn struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Values json.RawMessage `json:"values"`
	Meta   *struct {
		Unit []*struct {
			Name string `json:"name"`
		} `json:"unit"`
	} `json:"meta"`
}

type scalarResponse struct {
	Data struct {
		Attributes struct {
			Columns []scalarColumn `json:"columns"`
		} `json:"attributes"`
	} `json:"data"`
	Errors any `json:"errors"`
}

type scalarRow struct {
	tags  map[string]string
	value float64
}

func (c *Client) queryScalar(ctx context.Context, fromMs, toMs int64, formula string, queries map[string]string, groupBy []string, limit int) ([]scalarRow, string, error) {
	qs := make([]map[string]string, 0, len(queries))
	for _, name := range slices.Sorted(maps.Keys(queries)) {
		qs = append(qs, map[string]string{"name": name, "data_source": "metrics", "query": queries[name], "aggregator": "sum"})
	}
	f := map[string]any{"formula": formula}
	if limit > 0 {
		f["limit"] = map[string]any{"count": limit, "order": "desc"}
	}
	body := map[string]any{"data": map[string]any{
		"type":       "scalar_request",
		"attributes": map[string]any{"from": fromMs, "to": toMs, "formulas": []any{f}, "queries": qs},
	}}
	var resp scalarResponse
	if err := c.post(ctx, "/api/v2/query/scalar", body, &resp); err != nil {
		return nil, "", err
	}
	num := resp.formulaColumn(formula, queries)
	if num == nil {
		if msg := errorsText(resp.Errors); msg != "" {
			return nil, "", fmt.Errorf("datadog scalar query: %s", msg)
		}
		if len(resp.Data.Attributes.Columns) > 0 {
			return nil, "", fmt.Errorf("datadog scalar query returned no column for formula %q", formula)
		}
		return nil, "", nil
	}
	var values []*float64
	if err := json.Unmarshal(num.Values, &values); err != nil {
		return nil, "", fmt.Errorf("decoding scalar values: %w", err)
	}
	var groups []scalarColumn
	for _, col := range resp.Data.Attributes.Columns {
		if col.Type == "group" {
			groups = append(groups, col)
		}
	}
	cells := make([][][]string, len(groups))
	for i, g := range groups {
		cells[i] = groupCells(g.Values)
	}
	rows := make([]scalarRow, 0, len(values))
	for i, v := range values {
		if v == nil {
			continue
		}
		row := scalarRow{tags: map[string]string{}, value: *v}
		if len(groups) == 1 && len(groupBy) > 1 {
			cell := cellAt(cells[0], i)
			for j, tag := range groupBy {
				if j < len(cell) {
					row.tags[tag] = cell[j]
				}
			}
		} else {
			for j, g := range groups {
				name := g.Name
				if j < len(groupBy) && !slices.Contains(groupBy, name) {
					name = groupBy[j]
				}
				row.tags[name] = strings.Join(cellAt(cells[j], i), ",")
			}
		}
		rows = append(rows, row)
	}
	unit := ""
	if num.Meta != nil && len(num.Meta.Unit) > 0 && num.Meta.Unit[0] != nil {
		unit = num.Meta.Unit[0].Name
	}
	return rows, unit, nil
}

func (r *scalarResponse) formulaColumn(formula string, queries map[string]string) *scalarColumn {
	var fallback *scalarColumn
	for i := range r.Data.Attributes.Columns {
		col := &r.Data.Attributes.Columns[i]
		if col.Type != "number" {
			continue
		}
		if strings.ReplaceAll(col.Name, " ", "") == strings.ReplaceAll(formula, " ", "") {
			return col
		}
		if _, isQuery := queries[col.Name]; !isQuery {
			fallback = col
		}
	}
	return fallback
}

func groupCells(raw json.RawMessage) [][]string {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([][]string, len(items))
	for i, item := range items {
		var many []string
		if json.Unmarshal(item, &many) == nil {
			out[i] = many
			continue
		}
		var one string
		if json.Unmarshal(item, &one) == nil {
			out[i] = []string{one}
		}
	}
	return out
}

func cellAt(cells [][]string, i int) []string {
	if i < len(cells) {
		return cells[i]
	}
	return nil
}

func errorsText(v any) string {
	switch e := v.(type) {
	case string:
		return e
	case []any:
		parts := make([]string, 0, len(e))
		for _, item := range e {
			parts = append(parts, fmt.Sprint(item))
		}
		return strings.Join(parts, "; ")
	}
	return ""
}

type dbmRecord struct {
	at     time.Time
	host   string
	custom map[string]any
}

type dbmListResponse struct {
	Result *struct {
		Events []struct {
			Event struct {
				Timestamp any            `json:"timestamp"`
				Host      string         `json:"host"`
				Custom    map[string]any `json:"custom"`
			} `json:"event"`
		} `json:"events"`
	} `json:"result"`
	Status string `json:"status"`
	Errors any    `json:"errors"`
}

type dbmSamplesResult struct {
	query        string
	fromMs, toMs int64
	status       string
	read         int
	limit        int
	groups       []*dbmGroup
}

func (c *Client) dbmSamples(ctx context.Context, o DBMSamplesOptions) (*dbmSamplesResult, error) {
	query := strings.TrimSpace(o.Query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	fromMs, toMs, err := windowMs(o.From, o.To, time.Hour)
	if err != nil {
		return nil, err
	}
	recs, status, err := c.searchDBM(ctx, query, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	return &dbmSamplesResult{
		query:  query,
		fromMs: fromMs,
		toMs:   toMs,
		status: status,
		read:   len(recs),
		limit:  boundedOrDefault(o.Limit, defaultDBMGroups, maxDBMGroups),
		groups: groupDBMRecords(recs),
	}, nil
}

func (c *Client) searchDBM(ctx context.Context, query string, fromMs, toMs int64) ([]dbmRecord, string, error) {
	body := map[string]any{"list": map[string]any{
		"indexes": []string{"databasequery"},
		"limit":   dbmSamplesScan,
		"search":  map[string]string{"query": query},
		"sorts":   []any{map[string]any{"time": map[string]string{"order": "desc"}}},
		"time":    map[string]int64{"from": fromMs, "to": toMs},
	}}
	var resp dbmListResponse
	if err := c.postURL(ctx, c.appURL()+dbmSamplesPath, dbmSamplesPath, body, &resp); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusForbidden {
			return nil, "", fmt.Errorf("%w (DBM samples and plans need an application key without scopes, owned by a user who can read Database Monitoring)", err)
		}
		return nil, "", err
	}
	if resp.Result == nil {
		return nil, "", fmt.Errorf("datadog DBM search returned no result: %s", errorsText(resp.Errors))
	}
	recs := make([]dbmRecord, 0, len(resp.Result.Events))
	for _, e := range resp.Result.Events {
		custom := e.Event.Custom
		if custom == nil {
			custom = map[string]any{}
		}
		recs = append(recs, dbmRecord{
			at:     eventTime(e.Event.Timestamp, custom),
			host:   cmp.Or(e.Event.Host, attrString(custom, "host")),
			custom: custom,
		})
	}
	return recs, resp.Status, nil
}

func eventTime(v any, custom map[string]any) time.Time {
	switch t := v.(type) {
	case string:
		if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return ts.UTC()
		}
	case float64:
		return epochSecondsOrMillis(t)
	}
	if n, ok := attrNumber(custom, "timestamp"); ok {
		return epochSecondsOrMillis(n)
	}
	return time.Time{}
}

func epochSecondsOrMillis(v float64) time.Time {
	if v < 1e11 {
		return time.Unix(int64(v), 0).UTC()
	}
	return time.UnixMilli(int64(v)).UTC()
}

type dbmGroup struct {
	kind, signature, instance       string
	count                           int
	first, last                     time.Time
	maxDurNs                        float64
	longest                         *dbmRecord
	statement                       string
	users, apps, dbs, states, waits map[string]int
	maxRows                         float64
	hasRows                         bool
	minCost, maxCost                float64
	hasCost                         bool
	planSigs                        map[string]bool
	plan                            any
}

func groupDBMRecords(recs []dbmRecord) []*dbmGroup {
	index := map[string]*dbmGroup{}
	var groups []*dbmGroup
	for i := range recs {
		r := &recs[i]
		kind := attrString(r.custom, "dbm_type")
		sig := attrString(r.custom, "db.query_signature")
		inst := cmp.Or(attrString(r.custom, "database_instance"), r.host)
		key := kind + "\x00" + sig + "\x00" + inst
		g := index[key]
		if g == nil {
			g = &dbmGroup{
				kind: kind, signature: sig, instance: inst,
				users: map[string]int{}, apps: map[string]int{}, dbs: map[string]int{},
				states: map[string]int{}, waits: map[string]int{}, planSigs: map[string]bool{},
			}
			index[key] = g
			groups = append(groups, g)
		}
		g.add(r)
	}
	slices.SortStableFunc(groups, func(a, b *dbmGroup) int {
		return cmp.Or(cmp.Compare(b.maxDurNs, a.maxDurNs), cmp.Compare(b.count, a.count), b.last.Compare(a.last))
	})
	return groups
}

func (g *dbmGroup) add(r *dbmRecord) {
	g.count++
	if !r.at.IsZero() && (g.first.IsZero() || r.at.Before(g.first)) {
		g.first = r.at
	}
	if r.at.After(g.last) {
		g.last = r.at
	}
	if dur, _ := attrNumber(r.custom, "duration"); g.longest == nil || dur > g.maxDurNs {
		g.maxDurNs, g.longest = dur, r
	}
	if s := attrString(r.custom, "db.statement"); len(s) > len(g.statement) {
		g.statement = s
	}
	tally(g.users, attrString(r.custom, "db.user"))
	tally(g.apps, attrString(r.custom, "db.application"))
	tally(g.dbs, attrString(r.custom, "db.instance"))
	tally(g.states, cmp.Or(attrString(r.custom, "db.state"), attrString(r.custom, "postgres.state"), attrString(r.custom, "state")))
	tally(g.waits, strings.Trim(attrString(r.custom, "db.wait_event_type")+"/"+attrString(r.custom, "db.wait_event"), "/"))
	if rows, ok := attrNumber(r.custom, "db.rows"); ok {
		g.maxRows, g.hasRows = max(g.maxRows, rows), true
	}
	def := attrValue(r.custom, "db.plan.definition")
	if cost, ok := attrNumber(r.custom, "db.plan.cost"); ok {
		if !g.hasCost || cost < g.minCost {
			g.minCost = cost
		}
		if !g.hasCost || cost >= g.maxCost {
			g.maxCost = cost
			if def != nil {
				g.plan = def
			}
		}
		g.hasCost = true
	} else if g.plan == nil && def != nil {
		g.plan = def
	}
	if s := attrString(r.custom, "db.plan.signature"); s != "" {
		g.planSigs[s] = true
	}
}

func tally(m map[string]int, v string) {
	if v != "" {
		m[v]++
	}
}

func (r *dbmSamplesResult) format(siteLabel string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*[%s]* DBM samples · `%s` · %s → %s\n", siteLabel, r.query, formatMs(r.fromMs), formatMs(r.toMs))
	if r.status != "" && r.status != "done" {
		fmt.Fprintf(&sb, "Datadog marked this result %q, so it may be incomplete. Narrow the window or the query and retry.\n", r.status)
	}
	if r.read == 0 {
		sb.WriteString("No records matched. Check the query (dbm_type:activity for query samples, dbm_type:plan for explain plans), the window (samples are kept 15 days), and that Database Monitoring collects samples on these hosts.\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "Read %s, newest first · %s\n", plural(r.read, "record"), plural(len(r.groups), "group"))
	if r.read >= dbmSamplesScan {
		sb.WriteString("Stopped at the newest 1,000 records, so older ones in the window were not read. Narrow the query or the window to cover them.\n")
	}
	for i, g := range r.groups {
		if i == r.limit {
			fmt.Fprintf(&sb, "\n… %s not shown. Narrow the query to see them.\n", plural(len(r.groups)-r.limit, "more group"))
			break
		}
		g.write(&sb, i+1, i < dbmDetailGroups)
	}
	return sb.String()
}

func (g *dbmGroup) write(sb *strings.Builder, n int, detail bool) {
	fmt.Fprintf(sb, "\n%d. %s", n, cmp.Or(g.kind, "record"))
	if g.signature != "" {
		fmt.Fprintf(sb, " · `%s`", g.signature)
	}
	if g.instance != "" {
		fmt.Fprintf(sb, " · %s", g.instance)
	}
	head := []string{plural(g.count, "record"), timeSpan(g.first, g.last)}
	if g.maxDurNs > 0 {
		head = append([]string{"longest " + formatNanos(g.maxDurNs)}, head...)
	}
	fmt.Fprintf(sb, " — %s\n", strings.Join(head, " · "))

	var facts []string
	for _, f := range []struct {
		label  string
		values map[string]int
	}{{"db", g.dbs}, {"user", g.users}, {"app", g.apps}, {"state", g.states}, {"wait", g.waits}} {
		if s := topValues(f.values, 3); s != "" {
			facts = append(facts, f.label+" "+s)
		}
	}
	if g.hasRows {
		facts = append(facts, "rows up to "+formatCount(g.maxRows))
	}
	if len(facts) > 0 {
		fmt.Fprintf(sb, "   %s\n", strings.Join(facts, " · "))
	}
	if g.statement != "" {
		fmt.Fprintf(sb, "   %s\n", sqlExcerpt(g.statement, dbmSampleTextChars))
	}
	if g.hasCost || g.plan != nil {
		var parts []string
		switch {
		case g.hasCost && g.minCost != g.maxCost:
			parts = append(parts, "cost "+formatQuantity(g.minCost)+"–"+formatQuantity(g.maxCost))
		case g.hasCost:
			parts = append(parts, "cost "+formatQuantity(g.maxCost))
		}
		if len(g.planSigs) > 1 {
			parts = append(parts, fmt.Sprintf("%d distinct plans", len(g.planSigs)))
		}
		fmt.Fprintf(sb, "   %s\n", strings.TrimSpace("plan "+strings.Join(parts, " · ")))
		if detail {
			for _, line := range planLines(g.plan, dbmPlanNodes) {
				fmt.Fprintf(sb, "     %s\n", line)
			}
		}
	}
	if detail && g.longest != nil {
		if leaves := recordLeaves(g.longest.custom); len(leaves) > 0 {
			fmt.Fprintf(sb, "   longest record: %s\n", strings.Join(leaves, " "))
		}
	}
}

func topValues(m map[string]int, n int) string {
	if len(m) == 0 {
		return ""
	}
	keys := slices.SortedFunc(maps.Keys(m), func(a, b string) int {
		return cmp.Or(cmp.Compare(m[b], m[a]), cmp.Compare(a, b))
	})
	parts := make([]string, 0, n+1)
	for _, k := range keys[:min(n, len(keys))] {
		parts = append(parts, fmt.Sprintf("%s ×%s", k, formatCount(float64(m[k]))))
	}
	if len(keys) > n {
		parts = append(parts, fmt.Sprintf("+%d more", len(keys)-n))
	}
	return strings.Join(parts, ", ")
}

func recordLeaves(custom map[string]any) []string {
	var all []string
	flattenLogAttrs("", custom, &all)
	out := make([]string, 0, dbmDetailLeaves+1)
	skipped := 0
	for _, leaf := range all {
		path, _, _ := strings.Cut(leaf, ":")
		if slices.ContainsFunc(dbmShownAttrs, func(p string) bool { return strings.HasPrefix(path, p) }) {
			continue
		}
		if len(out) == dbmDetailLeaves {
			skipped++
			continue
		}
		out = append(out, leaf)
	}
	if skipped > 0 {
		out = append(out, fmt.Sprintf("(+%d more)", skipped))
	}
	return out
}

func planLines(def any, maxNodes int) []string {
	if s, ok := def.(string); ok {
		var parsed any
		if json.Unmarshal([]byte(s), &parsed) != nil {
			return nil
		}
		def = parsed
	}
	if arr, ok := def.([]any); ok && len(arr) > 0 {
		def = arr[0]
	}
	root, _ := def.(map[string]any)
	if p, ok := root["Plan"].(map[string]any); ok {
		root = p
	}
	if root == nil {
		return nil
	}
	var out []string
	truncated := false
	var walk func(node map[string]any, depth int)
	walk = func(node map[string]any, depth int) {
		if len(out) == maxNodes {
			truncated = true
			return
		}
		out = append(out, strings.Repeat("  ", depth)+planNode(node))
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if m, ok := child.(map[string]any); ok {
				walk(m, depth+1)
			}
		}
	}
	walk(root, 0)
	if truncated {
		out = append(out, "…")
	}
	return out
}

func planNode(n map[string]any) string {
	var b strings.Builder
	b.WriteString(cmp.Or(attrString(n, "Node Type"), "?"))
	if v := attrString(n, "Index Name"); v != "" {
		b.WriteString(" using " + v)
	}
	if v := attrString(n, "Relation Name"); v != "" {
		b.WriteString(" on " + v)
	}
	if v, ok := attrNumber(n, "Plan Rows"); ok {
		b.WriteString(" rows≈" + formatCount(v))
	}
	if v, ok := attrNumber(n, "Total Cost"); ok {
		b.WriteString(" cost " + formatQuantity(v))
	}
	if v, ok := attrNumber(n, "Actual Total Time"); ok {
		b.WriteString(" actual " + formatQuantity(v) + " ms")
	}
	if v, ok := attrNumber(n, "Actual Rows"); ok {
		b.WriteString(" actual rows " + formatCount(v))
	}
	if v, ok := attrNumber(n, "Rows Removed by Filter"); ok && v > 0 {
		b.WriteString(" removed by filter " + formatCount(v))
	}
	for _, k := range []string{"Index Cond", "Recheck Cond", "Hash Cond", "Merge Cond", "Join Filter", "Filter"} {
		if v := attrString(n, k); v != "" {
			fmt.Fprintf(&b, " %s: %s", k, text.Truncate(v, dbmPlanCondChars))
		}
	}
	if keys, ok := n["Sort Key"].([]any); ok && len(keys) > 0 {
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprint(k))
		}
		b.WriteString(" Sort Key: " + text.Truncate(strings.Join(parts, ", "), dbmPlanCondChars))
	}
	return b.String()
}

func sqlExcerpt(stmt string, limit int) string {
	s := strings.Join(strings.Fields(stmt), " ")
	if len(s) <= limit {
		return s
	}
	// A wide SELECT list would use the whole budget; shorten it so FROM and WHERE stay visible.
	if strings.HasPrefix(strings.ToUpper(s), "SELECT ") {
		if from := topLevelFrom(s); from > 80 {
			s = text.Truncate(s[:from], 60) + s[from:]
		}
	}
	return text.Truncate(s, limit)
}

func topLevelFrom(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ' ':
			if depth == 0 && i+6 <= len(s) && strings.EqualFold(s[i:i+6], " FROM ") {
				return i
			}
		}
	}
	return -1
}

func attrNumber(attrs map[string]any, path string) (float64, bool) {
	switch v := attrValue(attrs, path).(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

func windowMs(from, to string, def time.Duration) (int64, int64, error) {
	now := time.Now()
	f, err := parseTimeArg(from, now.Add(-def))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid from: %w", err)
	}
	t, err := parseTimeArg(to, now)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid to: %w", err)
	}
	if f >= t {
		return 0, 0, fmt.Errorf("from (%d) must be earlier than to (%d)", f, t)
	}
	return f * 1000, t * 1000, nil
}

func formatMs(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC")
}

func timeSpan(first, last time.Time) string {
	const layout = "2006-01-02 15:04:05"
	switch {
	case first.IsZero() && last.IsZero():
		return "time unknown"
	case first.Equal(last) || first.IsZero():
		return last.Format(layout) + " UTC"
	}
	return first.Format(layout) + " → " + last.Format(layout) + " UTC"
}

func formatNanos(ns float64) string {
	switch {
	case ns < 1e3:
		return fmt.Sprintf("%.0f ns", ns)
	case ns < 1e6:
		return fmt.Sprintf("%.1f µs", ns/1e3)
	case ns < 1e9:
		return fmt.Sprintf("%.1f ms", ns/1e6)
	case ns < 120e9:
		return fmt.Sprintf("%.1f s", ns/1e9)
	case ns < 3600e9:
		return fmt.Sprintf("%.1f min", ns/60e9)
	}
	return fmt.Sprintf("%.1f h", ns/3600e9)
}

func formatCount(v float64) string {
	s := strconv.FormatInt(int64(math.Round(math.Abs(v))), 10)
	var b strings.Builder
	if v < 0 {
		b.WriteByte('-')
	}
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func formatQuantity(v float64) string {
	if math.Abs(v) >= 1000 {
		return formatCount(v)
	}
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return formatCount(float64(n)) + " " + noun + "s"
}
