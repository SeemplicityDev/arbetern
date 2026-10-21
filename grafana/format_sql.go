package grafana

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/text"
)

func rangeLabel(from, to time.Time) string {
	return from.UTC().Format("2006-01-02 15:04") + " → " + to.UTC().Format("2006-01-02 15:04") + " UTC"
}

func dsHeading(sb *strings.Builder, ds Datasource) {
	fmt.Fprintf(sb, "\n*%s* (`%s`)", ds.Name, ds.UID)
}

func runFailed(sb *strings.Builder, run DatasourceRun, refs ...string) bool {
	if run.Err != nil {
		fmt.Fprintf(sb, " — Error: %v\n", run.Err)
		return true
	}
	for _, ref := range refs {
		if e := run.Results[ref].Err; e != "" {
			fmt.Fprintf(sb, " — Error: %s\n", text.Truncate(e, 800))
			return true
		}
	}
	return false
}

func missingTable(err string) bool {
	return strings.Contains(err, "Unknown table") || strings.Contains(err, "UNKNOWN_TABLE") || strings.Contains(err, "doesn't exist")
}

func humanCount(f float64) string {
	switch a := math.Abs(f); {
	case a >= 1e9:
		return fmt.Sprintf("%.1fB", f/1e9)
	case a >= 1e6:
		return fmt.Sprintf("%.1fM", f/1e6)
	case a >= 1e4:
		return fmt.Sprintf("%.1fK", f/1e3)
	}
	return formatFloat(f)
}

func humanBytes(f float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func oneLine(s string, max int) string {
	return text.Truncate(strings.Join(strings.Fields(s), " "), max)
}

// FormatSQL renders a QuerySQL result, one section per datasource.
func FormatSQL(res *SQLRunResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*ClickHouse query* — %s · %d %s\n", rangeLabel(res.From, res.To), len(res.Runs), plural(len(res.Runs), "datasource", "datasources"))
	if res.Limited {
		fmt.Fprintf(&sb, "_A LIMIT was added; at most %d rows are shown per datasource._\n", res.MaxRows)
	}
	for _, run := range res.Runs {
		dsHeading(&sb, run.Datasource)
		if runFailed(&sb, run, "A") {
			if sql := run.Results["A"].SQL; sql != "" && run.Results["A"].Err != unreachableNotice {
				fmt.Fprintf(&sb, "Executed query:\n```\n%s\n```\n", text.Truncate(sql, maxQueryChars))
			}
			continue
		}
		r := run.Results["A"]
		rows := r.Rows()
		shown := min(rows, res.MaxRows)
		fmt.Fprintf(&sb, " — %d %s\n", shown, plural(shown, "row", "rows"))
		for _, f := range r.Frames {
			for _, n := range f.Notices {
				fmt.Fprintf(&sb, "_Notice: %s_\n", text.Truncate(n, 300))
			}
			if f.Rows > res.MaxRows {
				f.Rows = res.MaxRows
			}
			if f.Rows > 0 {
				renderTable(&sb, f, res.MaxRows)
			}
		}
		if rows > res.MaxRows {
			fmt.Fprintf(&sb, "_More rows exist. Aggregate or filter in SQL, or raise max_rows (up to %d)._\n", maxSQLRows)
		}
		if rows == 0 {
			fmt.Fprintf(&sb, "_No rows. Executed query:_\n```\n%s\n```\n", text.Truncate(r.SQL, maxQueryChars))
		}
		if sb.Len() > maxOutputChars {
			sb.WriteString("_… output truncated; select fewer columns or rows._\n")
			break
		}
	}
	return text.Truncate(sb.String(), maxOutputChars+2000)
}

// FormatSchema renders a table list, or one table's columns.
func FormatSchema(res *SchemaResult) string {
	var sb strings.Builder
	ds := res.Datasource
	if res.Table != "" {
		fmt.Fprintf(&sb, "*%s* on %s (`%s`)\n", res.Table, ds.Name, ds.UID)
	} else {
		fmt.Fprintf(&sb, "*Tables — %s* (`%s`)\n", ds.Name, ds.UID)
	}
	if len(res.Unreachable) > 0 {
		fmt.Fprintf(&sb, "_Unreachable, skipped: %s._\n", strings.Join(res.Unreachable, ", "))
	}
	if len(res.Tables) == 0 {
		sb.WriteString("_No matching table is visible to this datasource's database user._\n")
		return sb.String()
	}
	for _, t := range res.Tables {
		fmt.Fprintf(&sb, "• `%s.%s` — %s", t.Database, t.Name, t.Engine)
		if t.Rows != nil {
			fmt.Fprintf(&sb, " · %s rows", humanCount(*t.Rows))
		}
		if t.Bytes != nil && *t.Bytes > 0 {
			fmt.Fprintf(&sb, " · %s", humanBytes(*t.Bytes))
		}
		if t.PartitionKey != "" {
			fmt.Fprintf(&sb, " · partition %s", t.PartitionKey)
		}
		if t.SortingKey != "" {
			fmt.Fprintf(&sb, " · order by %s", t.SortingKey)
		}
		if t.TTL != "" {
			fmt.Fprintf(&sb, " · TTL %s", t.TTL)
		}
		sb.WriteString("\n")
	}
	if res.Table != "" {
		fmt.Fprintf(&sb, "\n*Columns (%d)*\n", len(res.Columns))
		multiDB := len(res.Tables) > 1
		for _, col := range res.Columns {
			sb.WriteString("• ")
			if multiDB {
				sb.WriteString(col.Database + ".")
			}
			fmt.Fprintf(&sb, "`%s` %s", col.Name, col.Type)
			var keys []string
			if col.InSortingKey {
				keys = append(keys, "sort key")
			}
			if col.InPartitionKey {
				keys = append(keys, "partition key")
			}
			if len(keys) > 0 {
				fmt.Fprintf(&sb, " · %s", strings.Join(keys, ", "))
			}
			if col.Comment != "" {
				fmt.Fprintf(&sb, " — %s", oneLine(col.Comment, 160))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("_Filter on the sort key and partition columns, and always bound the time column, to keep queries cheap._\n")
	}
	return text.Truncate(sb.String(), maxOutputChars)
}

// FormatLogs renders a log search per datasource: total, breakdown and newest lines.
func FormatLogs(res *LogSearchResult) string {
	var sb strings.Builder
	s := res.Search
	fmt.Fprintf(&sb, "*Logs* — %s", rangeLabel(res.From, res.To))
	if f := logFilters(s); f != "" {
		fmt.Fprintf(&sb, " · %s", f)
	}
	sb.WriteString("\n")
	bodyMax := 400
	if s.Limit > 50 {
		bodyMax = 200
	}
	for _, run := range res.Runs {
		dsHeading(&sb, run.Datasource)
		if runFailed(&sb, run, "lines", "total") {
			continue
		}
		total := 0.0
		if rows := records(run.Results["total"]); len(rows) > 0 {
			total, _ = toFloat(rows[0]["n"])
		}
		lines := records(run.Results["lines"])
		fmt.Fprintf(&sb, " — %s matching %s\n", humanCount(total), plural(int(total), "line", "lines"))
		if total == 0 {
			continue
		}
		if counts := records(run.Results["counts"]); len(counts) > 0 {
			label := strings.ReplaceAll(strings.Join(res.CountBy, " · "), "`", "")
			parts := make([]string, 0, len(counts))
			for _, r := range counts {
				var keys []string
				for _, a := range res.CountBy {
					keys = append(keys, firstNonEmpty(asString(r[strings.Trim(a, "`")]), "(empty)"))
				}
				n, _ := toFloat(r["n"])
				parts = append(parts, strings.Join(keys, " · ")+" "+humanCount(n))
			}
			fmt.Fprintf(&sb, "By %s: %s\n", label, text.Truncate(strings.Join(parts, " | "), 3000))
		} else if e := run.Results["counts"].Err; e != "" {
			fmt.Fprintf(&sb, "_Breakdown failed: %s_\n", text.Truncate(e, 300))
		}
		fmt.Fprintf(&sb, "Newest %d:\n", len(lines))
		for _, r := range lines {
			fmt.Fprintf(&sb, "• %s %s [%s] %s", formatCell(r["ts"]), asString(r["service"]), asString(r["level"]), oneLine(asString(r["body"]), bodyMax))
			if id := asString(r["trace_id"]); id != "" {
				fmt.Fprintf(&sb, " · trace %s", id)
			}
			for _, f := range s.Fields {
				if v := asString(r[f]); v != "" {
					fmt.Fprintf(&sb, " · %s=%s", f, oneLine(v, 120))
				}
			}
			sb.WriteString("\n")
			if sb.Len() > maxOutputChars {
				sb.WriteString("_… output truncated; lower limit or narrow the filters._\n")
				return sb.String()
			}
		}
		if float64(len(lines)) < total {
			fmt.Fprintf(&sb, "_Showing %d of %s. Narrow the filters or the range, or raise limit (up to %d)._\n", len(lines), humanCount(total), maxLogLines)
		}
	}
	return sb.String()
}

func logFilters(s LogSearch) string {
	var parts []string
	if len(s.Services) > 0 {
		parts = append(parts, "service "+strings.Join(s.Services, ", "))
	}
	if len(s.Severities) > 0 {
		parts = append(parts, "severity "+strings.Join(s.Severities, ", "))
	}
	for _, t := range s.Text {
		parts = append(parts, fmt.Sprintf("text %q", t))
	}
	for _, t := range s.ExcludeText {
		parts = append(parts, fmt.Sprintf("not %q", t))
	}
	keys := make([]string, 0, len(s.Attributes))
	for k := range s.Attributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+s.Attributes[k])
	}
	if s.TraceID != "" {
		parts = append(parts, "trace "+s.TraceID)
	}
	return strings.Join(parts, " · ")
}

// FormatMetrics renders a metric listing or a metric's series.
func FormatMetrics(res *MetricResult) string {
	var sb strings.Builder
	q := res.Query
	if q.Metric == "" {
		fmt.Fprintf(&sb, "*Metrics* — %s", rangeLabel(res.From, res.To))
		if q.Search != "" {
			fmt.Fprintf(&sb, " · name contains %q", q.Search)
		}
		sb.WriteString("\n")
		for _, run := range res.Runs {
			dsHeading(&sb, run.Datasource)
			if run.Err != nil {
				fmt.Fprintf(&sb, " — Error: %v\n", run.Err)
				continue
			}
			var lines, errs []string
			for _, kind := range metricKinds {
				r := run.Results[kind]
				if r.Err != "" {
					if !missingTable(r.Err) {
						errs = append(errs, r.Err)
					}
					continue
				}
				for _, row := range records(r) {
					line := fmt.Sprintf("• `%s` (%s", asString(row["metric"]), kind)
					if u := asString(row["unit"]); u != "" {
						line += ", " + u
					}
					svc, _ := toFloat(row["services"])
					pts, _ := toFloat(row["points"])
					line += fmt.Sprintf(") — %s %s · %s points", humanCount(svc), plural(int(svc), "service", "services"), humanCount(pts))
					if d := asString(row["description"]); d != "" {
						line += " — " + oneLine(d, 120)
					}
					lines = append(lines, line)
				}
			}
			if len(errs) > 0 && len(lines) == 0 {
				fmt.Fprintf(&sb, " — Error: %s\n", text.Truncate(errs[0], 600))
				continue
			}
			sort.Strings(lines)
			fmt.Fprintf(&sb, " — %d %s\n", len(lines), plural(len(lines), "metric", "metrics"))
			for i, l := range lines {
				if i == 150 {
					fmt.Fprintf(&sb, "_… %d more; narrow with search or service._\n", len(lines)-i)
					break
				}
				sb.WriteString(l + "\n")
			}
		}
		return text.Truncate(sb.String(), maxOutputChars+2000)
	}
	fmt.Fprintf(&sb, "*Metric `%s`* — %s · interval %s", q.Metric, rangeLabel(res.From, res.To), intervalText(res.Interval))
	if len(q.GroupBy) > 0 {
		fmt.Fprintf(&sb, " · by %s", strings.Join(q.GroupBy, ", "))
	}
	sb.WriteString("\n")
	for _, run := range res.Runs {
		dsHeading(&sb, run.Datasource)
		if runFailed(&sb, run, "series") {
			continue
		}
		r := run.Results["series"]
		if note := res.Notes[run.Datasource.UID]; note != "" {
			fmt.Fprintf(&sb, " — %s", note)
		}
		sb.WriteString("\n")
		if r.Rows() == 0 {
			sb.WriteString("_No datapoints match these filters._\n")
			continue
		}
		for _, f := range r.Frames {
			renderSeriesFrame(&sb, f)
		}
		if sb.Len() > maxOutputChars {
			sb.WriteString("_… output truncated; group by fewer keys or filter._\n")
			break
		}
	}
	return text.Truncate(sb.String(), maxOutputChars+2000)
}

func renderSeriesFrame(sb *strings.Builder, f Frame) {
	timeIdx := -1
	var nums, labels []int
	for i, fld := range f.Fields {
		switch {
		case fld.Type == "time" && timeIdx < 0:
			timeIdx = i
		case fld.Type == "number":
			nums = append(nums, i)
		default:
			labels = append(labels, i)
		}
	}
	if timeIdx < 0 || len(nums) == 0 {
		renderTable(sb, f, 40)
		return
	}
	renderSeries(sb, f, timeIdx, nums, labels)
}

// FormatTraces renders one trace, or a span search with its summary.
func FormatTraces(res *TraceResult) string {
	var sb strings.Builder
	s := res.Search
	if s.TraceID != "" {
		fmt.Fprintf(&sb, "*Trace %s* — %s\n", s.TraceID, rangeLabel(res.From, res.To))
	} else {
		fmt.Fprintf(&sb, "*Spans* — %s\n", rangeLabel(res.From, res.To))
	}
	for _, run := range res.Runs {
		dsHeading(&sb, run.Datasource)
		if s.TraceID != "" {
			if runFailed(&sb, run, "trace") {
				continue
			}
			spans := records(run.Results["trace"])
			fmt.Fprintf(&sb, " — %d %s\n", len(spans), plural(len(spans), "span", "spans"))
			for _, r := range spans {
				fmt.Fprintf(&sb, "• %s %s · %s (%s) · %s ms · %s", formatCell(r["ts"]), asString(r["service"]), asString(r["span"]), asString(r["kind"]),
					formatCell(r["duration_ms"]), firstNonEmpty(asString(r["status"]), "unset"))
				if m := asString(r["status_message"]); m != "" {
					fmt.Fprintf(&sb, " — %s", oneLine(m, 200))
				}
				fmt.Fprintf(&sb, " · span %s", asString(r["span_id"]))
				if p := asString(r["parent_span_id"]); p != "" {
					fmt.Fprintf(&sb, " ← %s", p)
				}
				sb.WriteString("\n")
			}
			continue
		}
		if runFailed(&sb, run, "spans") {
			continue
		}
		spans := records(run.Results["spans"])
		summary := records(run.Results["summary"])
		fmt.Fprintf(&sb, " — %d %s shown\n", len(spans), plural(len(spans), "span", "spans"))
		if len(summary) > 0 {
			sb.WriteString("By operation (spans · errors · p50 / p95 / max ms):\n")
			for _, r := range summary {
				fmt.Fprintf(&sb, "• %s · %s — %s · %s · %s / %s / %s\n", asString(r["service"]), asString(r["span"]), formatCell(r["spans"]), formatCell(r["errors"]),
					formatCell(r["p50_ms"]), formatCell(r["p95_ms"]), formatCell(r["max_ms"]))
			}
		}
		if len(spans) > 0 {
			order := "Slowest"
			if strings.EqualFold(s.Order, "latest") {
				order = "Latest"
			}
			fmt.Fprintf(&sb, "%s spans:\n", order)
			for _, r := range spans {
				fmt.Fprintf(&sb, "• %s %s · %s · %s ms · %s · trace %s\n", formatCell(r["ts"]), asString(r["service"]), asString(r["span"]),
					formatCell(r["duration_ms"]), firstNonEmpty(asString(r["status"]), "unset"), asString(r["trace_id"]))
			}
		}
		if sb.Len() > maxOutputChars {
			sb.WriteString("_… output truncated; narrow the filters._\n")
			break
		}
	}
	return text.Truncate(sb.String(), maxOutputChars+2000)
}
