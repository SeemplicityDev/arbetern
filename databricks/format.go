package databricks

import (
	"fmt"
	"strings"

	"github.com/justmike1/arbetern/internal/text"
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
	sb.WriteString(text.Table{Headers: headers, Rows: r.Rows, Hint: text.HintSQL}.String())
	return sb.String()
}

// hostLabel strips the scheme off a workspace URL to keep the header short.
func hostLabel(host string) string {
	h := strings.TrimSpace(host)
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	return strings.TrimRight(h, "/")
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
