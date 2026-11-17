package clickhouse

import (
	"fmt"
	"sort"
	"strings"

	"github.com/justmike1/arbetern/internal/text"
)

// FormatUsageCost renders a UsageCostReport as a Slack-friendly summary: a
// headline grand total, a per-entity breakdown table (cost summed over the
// queried window, in ClickHouse Credits), and a by-category rollup. Mirrors the
// compact code-block style used by the other integration formatters.
func FormatUsageCost(r *UsageCostReport) string {
	if r == nil {
		return "No usage cost data."
	}

	var sb strings.Builder
	window := r.FromDate
	if r.ToDate != "" && r.ToDate != r.FromDate {
		window = r.FromDate + " → " + r.ToDate
	}
	fmt.Fprintf(&sb, "*ClickHouse Cloud usage cost — %s*\n", window)
	fmt.Fprintf(&sb, "Grand total: *%s*\n", money(r.GrandTotalCHC))
	if len(r.Filters) > 0 {
		fmt.Fprintf(&sb, "_Filters: %s_\n", strings.Join(r.Filters, ", "))
	}

	if len(r.Records) == 0 {
		sb.WriteString("\n_No usage in this period._\n")
		return sb.String()
	}

	// Aggregate cost per entity across the daily records, preserving first-seen
	// order before sorting so ties render deterministically.
	type entityAgg struct {
		name     string
		typ      string
		totalCHC float64
	}
	order := make([]string, 0)
	byEntity := make(map[string]*entityAgg)
	for i := range r.Records {
		rec := &r.Records[i]
		key := rec.EntityID + "|" + rec.EntityType
		agg, ok := byEntity[key]
		if !ok {
			name := rec.EntityName
			if strings.TrimSpace(name) == "" {
				name = rec.EntityID
			}
			agg = &entityAgg{name: name, typ: rec.EntityType}
			byEntity[key] = agg
			order = append(order, key)
		}
		agg.totalCHC += rec.TotalCHC
	}

	rows := make([]entityAgg, 0, len(byEntity))
	for _, key := range order {
		rows = append(rows, *byEntity[key])
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].totalCHC > rows[j].totalCHC })

	cells := make([][]string, 0, len(rows))
	for _, row := range rows {
		cells = append(cells, []string{row.name, row.typ, text.Commify(row.totalCHC)})
	}
	sb.WriteString(text.Table{
		Headers: []string{"Entity", "Type", "Cost (CHC)"},
		Rows:    cells,
		Align:   []text.Align{text.AlignLeft, text.AlignLeft, text.AlignRight},
		Hint:    "highest cost first",
	}.String())

	fmt.Fprintf(&sb, "_%d entit%s across %d daily record(s)_\n", len(rows), plural(len(rows)), len(r.Records))

	// By-category rollup across all records (only non-zero categories).
	var compute, storage, backup, dataTransfer, initialLoad, publicXfer, interRegion float64
	for i := range r.Records {
		m := r.Records[i].Metrics
		compute += m.ComputeCHC
		storage += m.StorageCHC
		backup += m.BackupCHC
		dataTransfer += m.DataTransferCHC
		initialLoad += m.InitialLoadCHC
		publicXfer += m.PublicDataTransferCHC
		interRegion += m.InterRegionTier1DataTransferCHC + m.InterRegionTier2DataTransferCHC +
			m.InterRegionTier3DataTransferCHC + m.InterRegionTier4DataTransferCHC
	}
	cats := []struct {
		label string
		chc   float64
	}{
		{"Compute", compute},
		{"Storage", storage},
		{"Backup", backup},
		{"Data transfer", dataTransfer},
		{"Initial load", initialLoad},
		{"Public data transfer", publicXfer},
		{"Inter-region data transfer", interRegion},
	}
	sort.SliceStable(cats, func(i, j int) bool { return cats[i].chc > cats[j].chc })
	var catLines []string
	for _, c := range cats {
		if c.chc <= 0 {
			continue
		}
		catLines = append(catLines, fmt.Sprintf("• %s: %s", c.label, money(c.chc)))
	}
	if len(catLines) > 0 {
		sb.WriteString("\n*By category*\n")
		sb.WriteString(strings.Join(catLines, "\n"))
		sb.WriteString("\n")
	}

	return sb.String()
}

// money renders a ClickHouse Credit amount with thousands separators and the
// CHC unit, e.g. 1234.5 → "1,234.50 CHC".
func money(v float64) string {
	return text.Commify(v) + " CHC"
}

// plural returns the "entit{y,ies}" suffix for n.
func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
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
		headers[i] = col.Name
	}
	sb.WriteString(text.Table{Headers: headers, Rows: r.Rows, Hint: text.HintSQL}.String())

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
