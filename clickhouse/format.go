package clickhouse

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// maxDisplayRows caps how many entity rows FormatUsageCost renders inline so
	// a big organization doesn't blow past Slack message limits.
	maxDisplayRows = 50
	// maxColWidth caps per-column width in the rendered table.
	maxColWidth = 40
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

	headers := []string{"Entity", "Type", "Cost (CHC)"}
	display := rows
	truncated := false
	if len(display) > maxDisplayRows {
		display = display[:maxDisplayRows]
		truncated = true
	}

	cells := make([][]string, 0, len(display))
	for _, row := range display {
		cells = append(cells, []string{
			truncate(row.name, maxColWidth),
			row.typ,
			commify(row.totalCHC),
		})
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

	fmt.Fprintf(&sb, "_%d entit%s across %d daily record(s)_\n", len(rows), plural(len(rows)), len(r.Records))
	if truncated {
		fmt.Fprintf(&sb, "_(showing top %d by cost)_\n", maxDisplayRows)
	}

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

// money renders a ClickHouse Credit amount with thousands separators and the
// CHC unit, e.g. 1234.5 → "1,234.50 CHC".
func money(v float64) string {
	return commify(v) + " CHC"
}

// commify formats a float with two decimal places and comma thousands
// separators, e.g. 1234567.5 → "1,234,567.50".
func commify(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	intPart, frac := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart, frac = s[:dot], s[dot:]
	}
	n := len(intPart)
	if n > 3 {
		var b strings.Builder
		pre := n % 3
		if pre > 0 {
			b.WriteString(intPart[:pre])
			if n > pre {
				b.WriteByte(',')
			}
		}
		for i := pre; i < n; i += 3 {
			b.WriteString(intPart[i : i+3])
			if i+3 < n {
				b.WriteByte(',')
			}
		}
		intPart = b.String()
	}
	out := intPart + frac
	if neg {
		out = "-" + out
	}
	return out
}

// plural returns the "entit{y,ies}" suffix for n.
func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// truncate shortens s to at most max characters, appending an ellipsis when it
// cuts.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
