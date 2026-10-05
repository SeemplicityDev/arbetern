package clickhouse

import (
	"fmt"
	"strings"

	"github.com/justmike1/arbetern/internal/text"
)

// maxColWidth caps per-column width in the rendered table.
const maxColWidth = 40

// writeRow renders one padded, space-separated table row.
func writeRow(sb *strings.Builder, cells []string, widths []int) {
	for i, w := range widths {
		val := ""
		if i < len(cells) {
			val = cells[i]
		}
		if i > 0 {
			sb.WriteString("  ")
		}
		fmt.Fprintf(sb, "%-*s", w, val)
	}
	sb.WriteString("\n")
}

// FormatSQLResult renders a query result as a Slack-friendly table. A scalar
// result (one row, one column — e.g. a count) is rendered as a one-line
// headline instead.
func FormatSQLResult(r *SQLQueryResult) string {
	if r == nil {
		return "No result."
	}
	if len(r.Columns) == 0 && len(r.Rows) == 0 {
		return "_Query returned no columns or rows._"
	}

	var sb strings.Builder

	// Scalar shortcut: one row, one column (e.g. a count).
	if len(r.Columns) == 1 && len(r.Rows) == 1 && len(r.Rows[0]) == 1 {
		fmt.Fprintf(&sb, "*%s* = `%s`\n", r.Columns[0].Name, r.Rows[0][0])
		return sb.String()
	}

	headers := make([]string, len(r.Columns))
	for i, col := range r.Columns {
		headers[i] = text.Truncate(col.Name, maxColWidth)
	}

	cells := make([][]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		out := make([]string, len(headers))
		for i := range headers {
			if i < len(row) {
				out[i] = text.Truncate(row[i], maxColWidth)
			}
		}
		cells = append(cells, out)
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range cells {
		for i := range row {
			if len(row[i]) > widths[i] {
				widths[i] = len(row[i])
			}
		}
	}

	sb.WriteString("```\n")
	writeRow(&sb, headers, widths)
	seps := make([]string, len(headers))
	for i := range seps {
		seps[i] = strings.Repeat("─", widths[i])
	}
	writeRow(&sb, seps, widths)
	for _, row := range cells {
		writeRow(&sb, row, widths)
	}
	sb.WriteString("```\n")

	noun := "rows"
	if r.RowCount == 1 {
		noun = "row"
	}
	fmt.Fprintf(&sb, "_%d %s_", r.RowCount, noun)
	if r.Truncated {
		sb.WriteString(" _(truncated)_")
	}
	sb.WriteString("\n")
	return sb.String()
}
