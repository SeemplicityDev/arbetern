package databricks

import (
	"fmt"
	"strings"
)

const (
	// maxDisplayRows caps how many rows FormatQueryResult renders inline so a
	// big result set doesn't blow past Slack message limits. The full set is
	// still available via ToCSV for snippet uploads.
	maxDisplayRows = 50
	// maxColWidth caps per-column width in the rendered table.
	maxColWidth = 40
)

// FormatQueryResult renders a QueryResult as a Slack-friendly fixed-width
// table inside a code block. Mirrors the compact style used by the aws/azure
// formatters so tool output looks consistent across integrations.
func FormatQueryResult(r *QueryResult) string {
	if r == nil || len(r.Columns) == 0 {
		return "Query executed successfully but returned no columns."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "*Databricks query — %d row(s)*\n", r.RowCount)
	if r.Truncated {
		sb.WriteString("_(result truncated — narrow the query or lower the row count)_\n")
	}
	if len(r.Rows) == 0 {
		sb.WriteString("\n_No rows returned._\n")
		return sb.String()
	}

	headers := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		headers[i] = c.Name
	}

	display := r.Rows
	if len(display) > maxDisplayRows {
		display = display[:maxDisplayRows]
	}

	// Compute column widths from headers + displayed rows.
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(truncate(h, maxColWidth))
	}
	for _, row := range display {
		for i := 0; i < len(headers) && i < len(row); i++ {
			if w := len(truncate(row[i], maxColWidth)); w > widths[i] {
				widths[i] = w
			}
		}
	}

	sb.WriteString("```\n")
	writeRow(&sb, headers, widths)
	// Separator line.
	seps := make([]string, len(headers))
	for i := range seps {
		seps[i] = strings.Repeat("─", widths[i])
	}
	writeRow(&sb, seps, widths)
	for _, row := range display {
		writeRow(&sb, row, widths)
	}
	sb.WriteString("```\n")

	if len(r.Rows) > maxDisplayRows {
		fmt.Fprintf(&sb, "_(showing first %d of %d rows — upload as a snippet for the full set)_\n", maxDisplayRows, len(r.Rows))
	}
	return sb.String()
}

// writeRow renders one padded, pipe-separated table row.
func writeRow(sb *strings.Builder, cells []string, widths []int) {
	for i, w := range widths {
		val := ""
		if i < len(cells) {
			val = truncate(cells[i], maxColWidth)
		}
		if i > 0 {
			sb.WriteString("  ")
		}
		fmt.Fprintf(sb, "%-*s", w, val)
	}
	sb.WriteString("\n")
}

// ToCSV renders the full result set (all rows) as RFC-4180-ish CSV. Useful for
// attaching large results to Slack as a downloadable snippet.
func ToCSV(r *QueryResult) string {
	if r == nil || len(r.Columns) == 0 {
		return ""
	}
	var sb strings.Builder
	header := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		header[i] = c.Name
	}
	sb.WriteString(csvLine(header))
	for _, row := range r.Rows {
		sb.WriteString(csvLine(row))
	}
	return sb.String()
}

func csvLine(cells []string) string {
	out := make([]string, len(cells))
	for i, c := range cells {
		if strings.ContainsAny(c, ",\"\n\r") {
			out[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
		} else {
			out[i] = c
		}
	}
	return strings.Join(out, ",") + "\n"
}

// truncate shortens s to at most max characters, appending an ellipsis when
// it cuts. Shared by the formatter and the client's error rendering.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
