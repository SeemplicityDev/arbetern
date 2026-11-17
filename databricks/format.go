package databricks

import (
	"fmt"
	"strings"
)

const (
	// maxPadWidth caps column padding; a longer cell is written whole and pushes the rest of its row right.
	maxPadWidth = 80
	// maxOutputBytes bounds the rendered table so one wide result can't flood the model's context.
	maxOutputBytes = 64 << 10
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
	// Name the source workspace so a report spanning several of them can't
	// attribute a result set to the wrong one.
	if h := hostLabel(r.Host); h != "" {
		fmt.Fprintf(&sb, "_workspace: %s_\n", h)
	}
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

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = min(len(h), maxPadWidth)
	}
	for _, row := range r.Rows {
		for i := 0; i < len(headers) && i < len(row); i++ {
			widths[i] = max(widths[i], min(len(row[i]), maxPadWidth))
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
	shown := 0
	for _, row := range r.Rows {
		if sb.Len() >= maxOutputBytes {
			break
		}
		writeRow(&sb, row, widths)
		shown++
	}
	sb.WriteString("```\n")

	if shown < len(r.Rows) {
		fmt.Fprintf(&sb, "_(showing first %d of %d rows; the rest exceed the output budget — aggregate in SQL or select fewer columns)_\n", shown, len(r.Rows))
	}
	return sb.String()
}

// hostLabel strips the scheme off a workspace URL to keep the header short.
func hostLabel(host string) string {
	h := strings.TrimSpace(host)
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	return strings.TrimRight(h, "/")
}

// writeRow renders one padded, pipe-separated table row.
func writeRow(sb *strings.Builder, cells []string, widths []int) {
	for i, w := range widths {
		val := ""
		if i < len(cells) {
			val = cells[i]
		}
		if i > 0 {
			sb.WriteString("  ")
		}
		if i == len(widths)-1 {
			sb.WriteString(val)
			continue
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
