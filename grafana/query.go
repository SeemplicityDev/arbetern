package grafana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxPanelsPerRun   = 8
	maxConcurrentRuns = 3
)

// RunOptions selects dashboard panels and the context to run their queries in.
type RunOptions struct {
	DashboardUID string
	PanelIDs     []int
	Start        string
	End          string
	Variables    map[string]string
	Interval     string
}

// RunResult is the outcome of one RunPanels call.
type RunResult struct {
	Dashboard *Dashboard
	From      time.Time
	To        time.Time
	Range     string
	Dropped   int
	Panels    []PanelRun
}

// PanelRun is one panel's outcome; Err is set when nothing could run.
type PanelRun struct {
	ID        int
	Panel     *Panel
	Link      string
	Interval  time.Duration
	Variables []string
	Queries   []QueryRun
	Err       error
}

// QueryRun is one target's outcome.
type QueryRun struct {
	RefID      string
	Datasource string
	Query      string
	Frames     []Frame
	Err        string
}

// RunPanels executes the saved queries of up to maxPanelsPerRun panels, the
// way the dashboard would, with the caller's time range and variable values.
// The time range defaults to the dashboard's own.
func (c *Client) RunPanels(ctx context.Context, opts RunOptions) (*RunResult, error) {
	d, err := c.GetDashboard(ctx, opts.DashboardUID)
	if err != nil {
		return nil, err
	}
	var ids []int
	seen := map[int]bool{}
	for _, id := range opts.PanelIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("pass at least one panel id")
	}
	dropped := 0
	if len(ids) > maxPanelsPerRun {
		dropped = len(ids) - maxPanelsPerRun
		ids = ids[:maxPanelsPerRun]
	}
	start, end := strings.TrimSpace(opts.Start), strings.TrimSpace(opts.End)
	if start == "" {
		start = firstNonEmpty(d.TimeFrom, "now-1h")
	}
	if end == "" {
		end = firstNonEmpty(d.TimeTo, "now")
	}
	from, to, err := TimeRange(start, end, time.Now())
	if err != nil {
		return nil, err
	}
	res := &RunResult{Dashboard: d, From: from, To: to, Range: start + " → " + end, Dropped: dropped, Panels: make([]PanelRun, len(ids))}
	runConcurrent(len(ids), maxConcurrentRuns, func(i int) {
		res.Panels[i] = c.runPanel(ctx, d, ids[i], from, to, opts)
	})
	return res, nil
}

func (c *Client) runPanel(ctx context.Context, d *Dashboard, id int, from, to time.Time, opts RunOptions) PanelRun {
	run := PanelRun{ID: id}
	p, err := d.Panel(id)
	if err != nil {
		run.Err = err
		return run
	}
	run.Panel = p
	run.Link = panelLink(d.URL, id, from, to, opts.Variables)
	if len(p.Targets) == 0 {
		if p.LibraryPanel != "" {
			run.Err = fmt.Errorf("panel %d is a library panel, whose queries this connector does not read", id)
		} else {
			run.Err = fmt.Errorf("panel %d (%s) has no queries", id, p.Type)
		}
		return run
	}
	run.Interval = panelInterval(p, from, to, opts.Interval)
	points := defaultMaxDataPoints
	if p.MaxDataPoints > 0 && p.MaxDataPoints < points {
		points = p.MaxDataPoints
	}
	sc := newScope(d, opts.Variables, from, to, run.Interval)

	// Hidden queries only run when an expression may depend on them.
	hasExpr := false
	for _, t := range p.Targets {
		if t.Datasource != nil && (t.Datasource.UID == "__expr__" || t.Datasource.Type == "__expr__") {
			hasExpr = true
		}
	}
	var queries []map[string]any
	usedRef := map[string]bool{}
	for i, t := range p.Targets {
		if t.Hide && !hasExpr {
			continue
		}
		ref := t.RefID
		if ref == "" || usedRef[ref] {
			ref = string(rune('A' + i%26))
			if i >= 26 {
				ref += strconv.Itoa(i / 26)
			}
		}
		usedRef[ref] = true
		qr := QueryRun{RefID: ref}
		q, label, err := c.buildQuery(ctx, sc, p, t, run.Interval, points)
		qr.Datasource = label
		if err != nil {
			qr.Err = err.Error()
			run.Queries = append(run.Queries, qr)
			continue
		}
		q["refId"] = ref
		qr.Query = queryExpression(q)
		run.Queries = append(run.Queries, qr)
		queries = append(queries, q)
	}
	run.Variables = sc.summary()
	if len(queries) == 0 {
		if len(run.Queries) == 0 {
			run.Err = fmt.Errorf("panel %d has only hidden queries", id)
		}
		return run
	}
	resp, err := c.dsQuery(ctx, from, to, queries)
	if err != nil {
		run.Err = err
		return run
	}
	for i := range run.Queries {
		qr := &run.Queries[i]
		if qr.Err != "" {
			continue
		}
		r, ok := resp.Results[qr.RefID]
		if !ok {
			qr.Err = "Grafana returned no result for this query"
			continue
		}
		if r.Error != "" {
			qr.Err = r.Error
		}
		for _, f := range r.Frames {
			qr.Frames = append(qr.Frames, decodeFrame(f))
		}
	}
	return run
}

// buildQuery turns one target into an /api/ds/query entry, returning the
// datasource label shown next to its results.
func (c *Client) buildQuery(ctx context.Context, sc *scope, p *Panel, t Target, interval time.Duration, points int) (map[string]any, string, error) {
	ref := t.Datasource
	if ref == nil || ref.UID == "" {
		if p.Datasource != nil {
			ref = p.Datasource
		}
	}
	if ref != nil && (ref.UID == "__expr__" || ref.Type == "__expr__") {
		sc.kind = ""
		q, _ := sc.interpolateTree(t.Raw).(map[string]any)
		q["datasource"] = map[string]any{"uid": "__expr__", "type": "__expr__"}
		return q, "expression", nil
	}
	uid, typ, label, err := c.resolveDatasource(ctx, sc, ref)
	if err != nil {
		return nil, label, err
	}
	sc.kind = datasourceKind(typ)
	clear(sc.missing)
	var q map[string]any
	if sc.kind == "clickhouse" {
		sql := firstNonEmpty(str(t.Raw, "rawSql"), t.Query)
		if strings.TrimSpace(sql) == "" {
			return nil, label, errors.New("this query has no SQL text (builder-only targets are not supported)")
		}
		// Raw rows (format 1) rather than the panel's own frame shaping keep
		// the result exactly what the SQL returns.
		q = map[string]any{
			"rawSql": sc.interpolate(sc.applyConditionalAll(sql)),
			"format": 1,
		}
	} else {
		q, _ = sc.interpolateTree(t.Raw).(map[string]any)
		if q == nil {
			q = map[string]any{}
		}
	}
	if missing := sc.missingNames(); len(missing) > 0 {
		return nil, label, fmt.Errorf("variable(s) %s have no saved selection: Grafana computes their options when the dashboard loads. Pass explicit values in variables, e.g. {%q: \"<value>\"}", strings.Join(missing, ", "), missing[0])
	}
	q["datasource"] = map[string]any{"uid": uid, "type": typ}
	q["intervalMs"] = interval.Milliseconds()
	q["maxDataPoints"] = points
	if t.Hide {
		q["hide"] = true
	}
	return q, label, nil
}

var dsVarRe = regexp.MustCompile(`\$\{(\w+)(?::[^}]*)?\}|\[\[(\w+)\]\]|\$(\w+)`)

// resolveDatasource turns a stored reference into a visible datasource:
// variables are resolved from the selection, names mapped to UIDs, and a
// missing reference means the instance default.
func (c *Client) resolveDatasource(ctx context.Context, sc *scope, ref *DSRef) (uid, typ, label string, err error) {
	if ref != nil {
		switch {
		case ref.UID == "-- Mixed --":
			return "", "", "mixed", errors.New("this query has no datasource of its own inside a mixed-datasource panel")
		case ref.UID == "-- Dashboard --" || ref.Type == "datasource":
			return "", "", "dashboard", errors.New("this panel reuses another panel's results; run that panel instead")
		case ref.UID == "grafana" || ref.UID == "-- Grafana --" || ref.Type == "grafana":
			return "", "", "grafana", errors.New("this query uses Grafana's built-in test datasource, which holds no real data")
		}
	}
	want := ""
	if ref != nil {
		want = ref.UID
	}
	if m := dsVarRe.FindStringSubmatch(want); m != nil {
		name := m[1] + m[2] + m[3]
		v := sc.vars[name]
		if v == nil {
			return "", "", want, fmt.Errorf("the datasource comes from variable %q, which the dashboard does not define; pass variables {%q: \"<datasource uid>\"}", name, name)
		}
		sc.used[name] = true
		if len(v.Values) > 0 && !v.IsAll {
			want = v.Values[0]
		} else {
			pluginType := v.Query
			if pluginType == "" && ref != nil {
				pluginType = ref.Type
			}
			ds, perr := c.pickDatasource(ctx, pluginType, v.Regex)
			if perr != nil {
				return "", "", want, perr
			}
			return ds.UID, ds.Type, ds.Name + " (" + ds.Type + ")", nil
		}
	}
	ds, ferr := c.findDatasource(ctx, want)
	if ferr == nil {
		return ds.UID, ds.Type, ds.Name + " (" + ds.Type + ")", nil
	}
	var ae *APIError
	if errors.As(ferr, &ae) && ref != nil && ref.Type != "" && want != "" {
		// The datasource list is unreadable; trust the dashboard's reference.
		return want, ref.Type, want + " (" + ref.Type + ")", nil
	}
	hint := ""
	if ref != nil && ref.Type != "" {
		if list, lerr := c.ListDatasources(ctx, ref.Type, ""); lerr == nil && len(list) > 0 {
			names := make([]string, 0, len(list))
			for i, d := range list {
				if i == 10 {
					break
				}
				names = append(names, d.UID)
			}
			hint = " Visible " + ref.Type + " datasources: " + strings.Join(names, ", ") + "."
		}
	}
	return "", "", want, errors.New(ferr.Error() + "." + hint)
}

func datasourceKind(typ string) string {
	t := strings.ToLower(typ)
	switch {
	case strings.Contains(t, "clickhouse"):
		return "clickhouse"
	case t == "postgres" || t == "grafana-postgresql-datasource" || t == "mysql" || t == "mssql":
		return "sql"
	case t == "prometheus":
		return "prometheus"
	case t == "loki":
		return "loki"
	}
	return ""
}

// interpolateTree interpolates every string in a target except its
// datasource reference and refId.
func (s *scope) interpolateTree(v any) any {
	switch t := v.(type) {
	case string:
		return s.interpolate(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if k == "datasource" || k == "refId" {
				continue
			}
			out[k] = s.interpolateTree(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = s.interpolateTree(x)
		}
		return out
	}
	return v
}

func (c *Client) dsQuery(ctx context.Context, from, to time.Time, queries []map[string]any) (*dsQueryResponse, error) {
	body := map[string]any{
		"queries": queries,
		"from":    strconv.FormatInt(from.UnixMilli(), 10),
		"to":      strconv.FormatInt(to.UnixMilli(), 10),
	}
	var resp dsQueryResponse
	err := c.do(ctx, http.MethodPost, "/api/ds/query", nil, body, &resp)
	var ae *APIError
	if errors.As(err, &ae) && len(ae.body) > 0 {
		// One failing query fails the whole response, but the body still
		// carries every query's result and error.
		var partial dsQueryResponse
		if json.Unmarshal(ae.body, &partial) == nil && len(partial.Results) > 0 {
			return &partial, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func panelLink(dashboardURL string, id int, from, to time.Time, vars map[string]string) string {
	if dashboardURL == "" {
		return ""
	}
	q := url.Values{}
	q.Set("viewPanel", "panel-"+strconv.Itoa(id))
	q.Set("from", strconv.FormatInt(from.UnixMilli(), 10))
	q.Set("to", strconv.FormatInt(to.UnixMilli(), 10))
	names := make([]string, 0, len(vars))
	for n := range vars {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, v := range strings.Split(vars[n], ",") {
			q.Add("var-"+strings.TrimPrefix(n, "var-"), strings.TrimSpace(v))
		}
	}
	return dashboardURL + "?" + q.Encode()
}

func runConcurrent(n, limit int, fn func(i int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
