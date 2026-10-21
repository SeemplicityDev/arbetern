package grafana

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Dashboard is a parsed classic (schema v1) dashboard.
type Dashboard struct {
	UID         string
	Title       string
	Description string
	Tags        []string
	URL         string
	FolderUID   string
	FolderTitle string
	Version     int
	Updated     string
	UpdatedBy   string
	Provisioned bool
	TimeFrom    string
	TimeTo      string
	Refresh     string
	Variables   []*Variable
	Panels      []*Panel
	// Model is the raw dashboard JSON, for JSONPath reads.
	Model map[string]any
}

// Panel is one non-row panel. Panels inside collapsed rows are included.
type Panel struct {
	ID            int
	Title         string
	Type          string
	Description   string
	Row           string
	Datasource    *DSRef
	Targets       []Target
	Interval      string
	MaxDataPoints int
	LibraryPanel  string
}

// DSRef is a datasource reference as a dashboard stores it. UID may hold a
// variable such as ${datasource}, or a name in older dashboards.
type DSRef struct {
	UID  string
	Type string
}

// Target is one query of a panel.
type Target struct {
	RefID      string
	Hide       bool
	Datasource *DSRef
	Query      string
	Raw        map[string]any
}

// Variable is one templating variable with its saved selection.
type Variable struct {
	Name       string
	Label      string
	Type       string
	Values     []string
	Multi      bool
	IsAll      bool
	IncludeAll bool
	AllValue   string
	Options    []string
	Query      string
	Regex      string
	Hidden     bool
	Filters    int
}

// GetDashboard reads and parses a dashboard by UID.
func (c *Client) GetDashboard(ctx context.Context, uid string) (*Dashboard, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, errors.New("pass a dashboard uid")
	}
	env, err := c.dashboards.Get(ctx, uid)
	if err != nil {
		return nil, err
	}
	return c.parseDashboard(uid, env), nil
}

func (c *Client) fetchDashboard(ctx context.Context, uid string) (*dashboardEnvelope, error) {
	var env dashboardEnvelope
	err := c.get(ctx, "/api/dashboards/uid/"+url.PathEscape(uid), nil, &env)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotAcceptable {
		return nil, fmt.Errorf("dashboard %s is stored in Grafana's v2 schema, which this connector does not read", uid)
	}
	if err != nil {
		return nil, err
	}
	if env.Dashboard == nil {
		return nil, fmt.Errorf("grafana returned no dashboard model for %s", uid)
	}
	if _, v2 := env.Dashboard["elements"]; v2 {
		return nil, fmt.Errorf("dashboard %s is stored in Grafana's v2 schema, which this connector does not read", uid)
	}
	return &env, nil
}

func (c *Client) parseDashboard(uid string, env *dashboardEnvelope) *Dashboard {
	m := env.Dashboard
	d := &Dashboard{
		UID:         uid,
		Title:       str(m, "title"),
		Description: str(m, "description"),
		Tags:        strs(m["tags"]),
		URL:         c.link(env.Meta.URL),
		FolderUID:   env.Meta.FolderUID,
		FolderTitle: env.Meta.FolderTitle,
		Version:     env.Meta.Version,
		Updated:     env.Meta.Updated,
		UpdatedBy:   env.Meta.UpdatedBy,
		Provisioned: env.Meta.Provisioned,
		TimeFrom:    str(obj(m, "time"), "from"),
		TimeTo:      str(obj(m, "time"), "to"),
		Refresh:     str(m, "refresh"),
		Model:       m,
	}
	if d.URL == "" {
		d.URL = c.link("/d/" + url.PathEscape(uid))
	}
	for _, v := range arr(obj(m, "templating"), "list") {
		if vm, ok := v.(map[string]any); ok && str(vm, "name") != "" {
			d.Variables = append(d.Variables, parseVariable(vm))
		}
	}
	if panels, ok := m["panels"].([]any); ok {
		row := ""
		for _, p := range panels {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if str(pm, "type") == "row" {
				row = str(pm, "title")
				for _, np := range arr(pm, "panels") {
					if nm, ok := np.(map[string]any); ok {
						d.Panels = append(d.Panels, parsePanel(nm, row))
					}
				}
				continue
			}
			d.Panels = append(d.Panels, parsePanel(pm, row))
		}
	} else {
		for _, r := range arr(m, "rows") {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			for _, np := range arr(rm, "panels") {
				if nm, ok := np.(map[string]any); ok {
					d.Panels = append(d.Panels, parsePanel(nm, str(rm, "title")))
				}
			}
		}
	}
	return d
}

// Panel returns the panel with the given ID.
func (d *Dashboard) Panel(id int) (*Panel, error) {
	for _, p := range d.Panels {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, fmt.Errorf("dashboard %s has no panel %d", d.UID, id)
}

// Property evaluates a JSONPath expression against the dashboard model.
func (d *Dashboard) Property(path string) (any, error) {
	return evalJSONPath(d.Model, path)
}

// Variable returns the variable with the given name, or nil.
func (d *Dashboard) Variable(name string) *Variable {
	for _, v := range d.Variables {
		if v.Name == name {
			return v
		}
	}
	return nil
}

func parsePanel(m map[string]any, row string) *Panel {
	p := &Panel{
		ID:            num(m, "id"),
		Title:         str(m, "title"),
		Type:          str(m, "type"),
		Description:   str(m, "description"),
		Row:           row,
		Datasource:    parseDSRef(m["datasource"]),
		Interval:      str(m, "interval"),
		MaxDataPoints: num(m, "maxDataPoints"),
		LibraryPanel:  str(obj(m, "libraryPanel"), "uid"),
	}
	for _, t := range arr(m, "targets") {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		hide, _ := tm["hide"].(bool)
		p.Targets = append(p.Targets, Target{
			RefID:      str(tm, "refId"),
			Hide:       hide,
			Datasource: parseDSRef(tm["datasource"]),
			Query:      queryExpression(tm),
			Raw:        tm,
		})
	}
	return p
}

func parseDSRef(v any) *DSRef {
	switch ds := v.(type) {
	case map[string]any:
		ref := &DSRef{UID: str(ds, "uid"), Type: str(ds, "type")}
		if ref.UID == "" && ref.Type == "" {
			return nil
		}
		return ref
	case string:
		if ds == "" {
			return nil
		}
		return &DSRef{UID: ds}
	}
	return nil
}

// queryExpression returns the query text of a target: the first non-empty of
// the fields the common datasources keep it in.
func queryExpression(t map[string]any) string {
	for _, k := range []string{"expr", "rawSql", "rawSQL", "query", "expression", "rawQuery", "target"} {
		if s := str(t, k); s != "" {
			return s
		}
	}
	return ""
}

func parseVariable(m map[string]any) *Variable {
	v := &Variable{
		Name:       str(m, "name"),
		Label:      str(m, "label"),
		Type:       str(m, "type"),
		IncludeAll: boolean(m, "includeAll"),
		AllValue:   str(m, "allValue"),
		Multi:      boolean(m, "multi"),
		Regex:      str(m, "regex"),
		Hidden:     num(m, "hide") == 2,
		Filters:    len(arr(m, "filters")),
	}
	switch q := m["query"].(type) {
	case string:
		v.Query = q
	case map[string]any:
		v.Query = queryExpression(q)
	}
	for _, o := range arr(m, "options") {
		if om, ok := o.(map[string]any); ok {
			if val := str(om, "value"); val != "" && val != allMarker {
				v.Options = append(v.Options, val)
			}
		}
	}
	current := obj(m, "current")
	switch val := current["value"].(type) {
	case string:
		v.Values = []string{val}
	case []any:
		v.Multi = true
		v.Values = strs(val)
	default:
		if t := strs(current["text"]); len(t) > 0 {
			v.Values = t
		}
	}
	if len(v.Values) > 0 && (v.Values[0] == allMarker || (current["value"] == nil && v.Values[0] == "All" && v.IncludeAll)) {
		v.IsAll, v.Values = true, nil
	}
	if len(v.Values) == 0 && !v.IsAll {
		switch v.Type {
		case "constant", "textbox":
			if v.Query != "" {
				v.Values = []string{v.Query}
			}
		case "custom":
			if len(v.Options) > 0 {
				v.Values = v.Options[:1]
			}
		}
	}
	return v
}

// ── JSON helpers ───────────────────────────────────────────────────────────

func obj(m map[string]any, k string) map[string]any {
	if m == nil {
		return nil
	}
	o, _ := m[k].(map[string]any)
	return o
}

func arr(m map[string]any, k string) []any {
	if m == nil {
		return nil
	}
	a, _ := m[k].([]any)
	return a
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func num(m map[string]any, k string) int {
	if m == nil {
		return 0
	}
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func boolean(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

func strs(v any) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			switch s := x.(type) {
			case string:
				out = append(out, s)
			case float64:
				out = append(out, strconv.FormatFloat(s, 'f', -1, 64))
			}
		}
		return out
	}
	return nil
}
