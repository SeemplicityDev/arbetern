package grafana

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/text"
)

const (
	// maxOutputChars caps one tool result so a large dashboard cannot flood
	// the model's context.
	maxOutputChars = 24000
	maxQueryChars  = 1500
	maxPropChars   = 16000
)

// FormatSearch renders dashboard or folder search hits.
func FormatSearch(kind, query string, hits []SearchHit, limit int) string {
	var sb strings.Builder
	label := "dashboards"
	if kind == "dash-folder" {
		label = "folders"
	}
	fmt.Fprintf(&sb, "*Grafana %s", label)
	if query != "" {
		fmt.Fprintf(&sb, " matching %q", query)
	}
	fmt.Fprintf(&sb, " (%d)*\n", len(hits))
	if len(hits) == 0 {
		sb.WriteString("_Nothing matched. Try a shorter title fragment, or drop the filters._\n")
		return sb.String()
	}
	for _, h := range hits {
		fmt.Fprintf(&sb, "• *%s* — uid `%s`", text.Truncate(h.Title, 120), h.UID)
		if h.FolderTitle != "" {
			fmt.Fprintf(&sb, " · folder %s (`%s`)", h.FolderTitle, h.FolderUID)
		}
		if len(h.Tags) > 0 {
			fmt.Fprintf(&sb, " · tags %s", strings.Join(h.Tags, ", "))
		}
		if h.URL != "" {
			fmt.Fprintf(&sb, " · <%s|open>", h.URL)
		}
		sb.WriteString("\n")
		if h.Description != "" {
			fmt.Fprintf(&sb, "   _%s_\n", text.Truncate(h.Description, 200))
		}
	}
	if limit > 0 && len(hits) >= limit {
		sb.WriteString("_More may exist: pass page+1, or narrow the query._\n")
	}
	return sb.String()
}

// FormatDatasources renders the datasource list with the UIDs panels use.
func FormatDatasources(list []Datasource) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Grafana datasources (%d)*\n", len(list))
	if len(list) == 0 {
		sb.WriteString("_No datasource matched, or the service account may not query any._\n")
		return sb.String()
	}
	for _, d := range list {
		fmt.Fprintf(&sb, "• *%s* — uid `%s` · %s", d.Name, d.UID, d.Type)
		if d.TypeName != "" && !strings.EqualFold(d.TypeName, d.Type) {
			fmt.Fprintf(&sb, " (%s)", d.TypeName)
		}
		if d.IsDefault {
			sb.WriteString(" · _default_")
		}
		if d.Database != "" {
			fmt.Fprintf(&sb, " · database %s", d.Database)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// FormatSummary renders a dashboard overview: metadata, variables and panels.
func FormatSummary(d *Dashboard) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%s* — uid `%s`", d.Title, d.UID)
	if d.FolderTitle != "" {
		fmt.Fprintf(&sb, " · folder %s", d.FolderTitle)
	}
	fmt.Fprintf(&sb, " · <%s|open>\n", d.URL)
	var meta []string
	if len(d.Tags) > 0 {
		meta = append(meta, "tags "+strings.Join(d.Tags, ", "))
	}
	if d.TimeFrom != "" {
		meta = append(meta, "default range "+d.TimeFrom+" → "+firstNonEmpty(d.TimeTo, "now"))
	}
	if d.Refresh != "" {
		meta = append(meta, "refresh "+d.Refresh)
	}
	if d.Version > 0 {
		v := fmt.Sprintf("version %d", d.Version)
		if d.Updated != "" {
			v += ", updated " + shortDate(d.Updated)
		}
		if d.UpdatedBy != "" {
			v += " by " + d.UpdatedBy
		}
		meta = append(meta, v)
	}
	if d.Provisioned {
		meta = append(meta, "provisioned from code")
	}
	if len(meta) > 0 {
		fmt.Fprintf(&sb, "_%s_\n", strings.Join(meta, " · "))
	}
	if d.Description != "" {
		fmt.Fprintf(&sb, "%s\n", text.Truncate(d.Description, 600))
	}
	if len(d.Variables) > 0 {
		fmt.Fprintf(&sb, "\n*Variables (%d)*\n", len(d.Variables))
		for _, v := range d.Variables {
			sb.WriteString(formatVariable(v))
		}
	}
	fmt.Fprintf(&sb, "\n*Panels (%d)*\n", len(d.Panels))
	row := "\x00"
	for _, p := range d.Panels {
		if p.Row != row {
			row = p.Row
			if row != "" {
				fmt.Fprintf(&sb, "_Row: %s_\n", row)
			}
		}
		fmt.Fprintf(&sb, "• %d · *%s* (%s)", p.ID, text.Truncate(firstNonEmpty(p.Title, "untitled"), 100), p.Type)
		switch {
		case p.LibraryPanel != "" && len(p.Targets) == 0:
			sb.WriteString(" · library panel")
		case len(p.Targets) > 0:
			fmt.Fprintf(&sb, " · %d %s", len(p.Targets), plural(len(p.Targets), "query", "queries"))
			if ds := panelDatasource(p); ds != "" {
				fmt.Fprintf(&sb, " on %s", ds)
			}
		}
		sb.WriteString("\n")
		if sb.Len() > maxOutputChars {
			sb.WriteString("_… truncated; use grafana_get_dashboard_property for the rest._\n")
			break
		}
	}
	return sb.String()
}

func formatVariable(v *Variable) string {
	kind := v.Type
	var flags []string
	if v.Multi {
		flags = append(flags, "multi")
	}
	if v.IncludeAll {
		flags = append(flags, "include All")
	}
	if v.Hidden {
		flags = append(flags, "hidden")
	}
	if len(flags) > 0 {
		kind += ", " + strings.Join(flags, ", ")
	}
	val := strings.Join(v.Values, ", ")
	switch {
	case v.IsAll:
		val = "All"
		if v.AllValue != "" {
			val += " (" + v.AllValue + ")"
		}
	case v.Type == "adhoc":
		val = fmt.Sprintf("%d filter(s)", v.Filters)
	case val == "" && v.Type == "query":
		val = "_no saved selection_"
	case val == "":
		val = `""`
	}
	line := fmt.Sprintf("• `%s` (%s) = %s", v.Name, kind, text.Truncate(val, 200))
	switch {
	case v.Type == "datasource" && v.Query != "":
		line += " · picks a " + v.Query + " datasource"
	case v.Type == "query" && v.Query != "":
		line += " · options from: " + text.Truncate(strings.Join(strings.Fields(v.Query), " "), 160)
	case v.Type == "custom" && len(v.Options) > 0 && strings.Join(v.Options, ",") != strings.Join(v.Values, ","):
		line += " · options: " + text.Truncate(strings.Join(v.Options, ", "), 160)
	}
	return line + "\n"
}

func panelDatasource(p *Panel) string {
	seen := map[string]bool{}
	var out []string
	add := func(r *DSRef) {
		if r == nil || r.UID == "" || seen[r.UID] || r.UID == "-- Mixed --" {
			return
		}
		seen[r.UID] = true
		out = append(out, "`"+r.UID+"`")
	}
	add(p.Datasource)
	for _, t := range p.Targets {
		add(t.Datasource)
	}
	return strings.Join(out, ", ")
}

// FormatPanelQueries renders the saved queries of one panel, or of every
// panel, with the variable selections they reference.
func FormatPanelQueries(d *Dashboard, panelID int) string {
	var sb strings.Builder
	panels := d.Panels
	limit := maxQueryChars
	if panelID > 0 {
		p, err := d.Panel(panelID)
		if err != nil {
			return "Error: " + err.Error()
		}
		panels = []*Panel{p}
		limit = maxOutputChars / 2
	}
	fmt.Fprintf(&sb, "*Queries — %s* (uid `%s`)\n", d.Title, d.UID)
	referenced := map[string]bool{}
	for _, p := range panels {
		if len(p.Targets) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "\n*%d · %s* (%s)", p.ID, text.Truncate(firstNonEmpty(p.Title, "untitled"), 100), p.Type)
		if p.Row != "" {
			fmt.Fprintf(&sb, " · row %s", p.Row)
		}
		sb.WriteString("\n")
		for _, t := range p.Targets {
			ref := t.Datasource
			if ref == nil || ref.UID == "" {
				ref = p.Datasource
			}
			fmt.Fprintf(&sb, "%s", firstNonEmpty(t.RefID, "?"))
			if ref != nil {
				fmt.Fprintf(&sb, " · datasource `%s`", ref.UID)
				if ref.Type != "" {
					fmt.Fprintf(&sb, " (%s)", ref.Type)
				}
			} else {
				sb.WriteString(" · default datasource")
			}
			if t.Hide {
				sb.WriteString(" · hidden")
			}
			sb.WriteString("\n")
			q := t.Query
			if q == "" {
				b, _ := json.Marshal(t.Raw)
				q = string(b)
			}
			for _, m := range templateVarRe.FindAllStringSubmatch(q+" "+refUID(ref), -1) {
				if name := m[1] + m[2] + m[4]; !strings.HasPrefix(name, "__") {
					referenced[name] = true
				}
			}
			fmt.Fprintf(&sb, "```\n%s\n```\n", text.Truncate(strings.TrimSpace(q), limit))
		}
		if sb.Len() > maxOutputChars {
			sb.WriteString("_… truncated; pass panel_id for one panel's full queries._\n")
			break
		}
	}
	var lines []string
	for _, v := range d.Variables {
		if referenced[v.Name] {
			lines = append(lines, strings.TrimSuffix(formatVariable(v), "\n"))
		}
	}
	if len(lines) > 0 {
		sb.WriteString("\n*Variables these queries use*\n")
		sb.WriteString(strings.Join(lines, "\n"))
		sb.WriteString("\n")
	}
	sb.WriteString("_Macros such as $__timeFilter and $__timeInterval are expanded by the datasource when the query runs. Run panels with grafana_run_panel_query._\n")
	return sb.String()
}

func refUID(r *DSRef) string {
	if r == nil {
		return ""
	}
	return r.UID
}

// FormatProperty renders the value a JSONPath expression selected.
func FormatProperty(d *Dashboard, path string, v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error encoding the result: %v", err)
	}
	out := string(b)
	note := ""
	if len(out) > maxPropChars {
		out = text.TruncatePlain(out, maxPropChars)
		note = fmt.Sprintf("\n_… truncated at %d of %d characters; select a narrower path._", maxPropChars, len(b))
	}
	return fmt.Sprintf("*%s* (uid `%s`) · `%s`\n```\n%s\n```%s\n", d.Title, d.UID, path, out, note)
}

// FormatAnnotations renders annotations newest first.
func FormatAnnotations(list []Annotation) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Grafana annotations (%d)*\n", len(list))
	if len(list) == 0 {
		sb.WriteString("_None in this window. Widen the time range or drop filters._\n")
		return sb.String()
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Time > list[j].Time })
	for _, a := range list {
		fmt.Fprintf(&sb, "• %s", formatTime(time.UnixMilli(a.Time).UTC()))
		if a.TimeEnd > a.Time {
			fmt.Fprintf(&sb, " → %s", formatTime(time.UnixMilli(a.TimeEnd).UTC()))
		}
		if a.AlertName != "" {
			fmt.Fprintf(&sb, " · alert %s", a.AlertName)
			if a.NewState != "" {
				fmt.Fprintf(&sb, " → %s", a.NewState)
			}
		}
		if a.Text != "" {
			fmt.Fprintf(&sb, " — %s", text.Truncate(strings.Join(strings.Fields(text.StripHTML(a.Text)), " "), 300))
		}
		if len(a.Tags) > 0 {
			fmt.Fprintf(&sb, " · tags %s", strings.Join(a.Tags, ", "))
		}
		if a.DashboardUID != "" {
			fmt.Fprintf(&sb, " · dashboard `%s`", a.DashboardUID)
			if a.PanelID > 0 {
				fmt.Fprintf(&sb, " panel %d", a.PanelID)
			}
		}
		if a.Login != "" {
			fmt.Fprintf(&sb, " · by %s", a.Login)
		}
		sb.WriteString("\n")
		if sb.Len() > maxOutputChars {
			sb.WriteString("_… truncated; narrow the window or filters._\n")
			break
		}
	}
	return sb.String()
}

// FormatRun renders panel results: per panel its datasource, each query's
// rows or series summary, and errors next to the query that caused them.
func FormatRun(res *RunResult) string {
	var sb strings.Builder
	d := res.Dashboard
	fmt.Fprintf(&sb, "*%s* (uid `%s`) — %s → %s UTC (%s)\n", d.Title, d.UID,
		res.From.UTC().Format("2006-01-02 15:04"), res.To.UTC().Format("2006-01-02 15:04"), res.Range)
	if res.Dropped > 0 {
		fmt.Fprintf(&sb, "_Only the first %d panels ran; %d more were skipped. Call again for the rest._\n", maxPanelsPerRun, res.Dropped)
	}
	maxRows := 40
	if n := len(res.Panels); n > 1 {
		maxRows = max(10, 60/n)
	}
	for _, pr := range res.Panels {
		sb.WriteString("\n")
		title := fmt.Sprintf("panel %d", pr.ID)
		if pr.Panel != nil {
			title = fmt.Sprintf("%d · %s", pr.ID, firstNonEmpty(pr.Panel.Title, "untitled"))
		}
		fmt.Fprintf(&sb, "*%s*", title)
		if pr.Panel != nil {
			fmt.Fprintf(&sb, " (%s)", pr.Panel.Type)
		}
		if pr.Link != "" {
			fmt.Fprintf(&sb, " · <%s|open in Grafana>", pr.Link)
		}
		sb.WriteString("\n")
		if pr.Err != nil {
			fmt.Fprintf(&sb, "Error: %v\n", pr.Err)
			continue
		}
		meta := []string{"interval " + intervalText(pr.Interval)}
		if len(pr.Variables) > 0 {
			meta = append(meta, "variables "+strings.Join(pr.Variables, ", "))
		}
		fmt.Fprintf(&sb, "_%s_\n", text.Truncate(strings.Join(meta, " · "), 600))
		for _, q := range pr.Queries {
			fmt.Fprintf(&sb, "Query %s", q.RefID)
			if q.Datasource != "" {
				fmt.Fprintf(&sb, " on %s", q.Datasource)
			}
			rows := 0
			for _, f := range q.Frames {
				rows += f.Rows
			}
			if q.Err == "" {
				fmt.Fprintf(&sb, " — %d %s", rows, plural(rows, "row", "rows"))
			}
			sb.WriteString("\n")
			if q.Err != "" {
				fmt.Fprintf(&sb, "Error: %s\n", text.Truncate(q.Err, 800))
			}
			for i, f := range q.Frames {
				if i == 10 {
					fmt.Fprintf(&sb, "_… %d more frames not shown._\n", len(q.Frames)-i)
					break
				}
				for _, n := range f.Notices {
					fmt.Fprintf(&sb, "_Notice: %s_\n", text.Truncate(n, 300))
				}
				if len(q.Frames) > 1 && f.Name != "" {
					fmt.Fprintf(&sb, "_frame %s_\n", f.Name)
				}
				renderFrame(&sb, f, maxRows)
			}
			if q.Err != "" || rows == 0 {
				executed := q.Query
				for _, f := range q.Frames {
					if f.Executed != "" {
						executed = f.Executed
						break
					}
				}
				if rows == 0 && q.Err == "" {
					sb.WriteString("_No rows for this range and selection. Try a wider start (e.g. now-24h) or different variable values._\n")
				}
				if executed != "" {
					fmt.Fprintf(&sb, "Executed query:\n```\n%s\n```\n", text.Truncate(strings.TrimSpace(executed), maxQueryChars))
				}
			}
			if sb.Len() > maxOutputChars {
				break
			}
		}
		if sb.Len() > maxOutputChars {
			sb.WriteString("_… output truncated; run fewer panels per call or narrow the time range._\n")
			break
		}
	}
	return text.Truncate(sb.String(), maxOutputChars+2000)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func shortDate(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	return s
}
