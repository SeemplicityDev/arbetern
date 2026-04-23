package aws

import (
	"fmt"
	"sort"
	"strings"
)

// FormatCostAndUsage returns a Slack-friendly rendering of a CostAndUsageResult.
// It's intentionally compact: a header line, then one line per period, with
// grouped periods unfolded into a sorted list below each period's total.
func FormatCostAndUsage(r *CostAndUsageResult) string {
	if r == nil || len(r.Periods) == 0 {
		return "No cost data returned for the requested window."
	}
	var sb strings.Builder
	unit := firstUnit(r.Periods)
	fmt.Fprintf(&sb, "*AWS %s cost — %s (%s → %s)*\n", strings.ToLower(r.Granularity), r.Metric, r.Start, r.End)
	if r.GroupBy != "" {
		fmt.Fprintf(&sb, "_Grouped by %s_\n", r.GroupBy)
	}
	sb.WriteString("```\n")
	fmt.Fprintf(&sb, "%-12s  %12s\n", "period", unit)
	sb.WriteString(strings.Repeat("─", 28) + "\n")
	for _, p := range r.Periods {
		fmt.Fprintf(&sb, "%-12s  %12s\n", p.Start, money(p.Total, p.Unit))
	}
	sb.WriteString("```\n")
	if r.GroupBy != "" {
		// Show groups under each period (top 10 by cost, since some dimensions
		// like USAGE_TYPE can return hundreds of rows that would bury Slack).
		for _, p := range r.Periods {
			if len(p.Groups) == 0 {
				continue
			}
			type kv struct {
				k string
				v float64
			}
			sorted := make([]kv, 0, len(p.Groups))
			for k, v := range p.Groups {
				sorted = append(sorted, kv{k, v})
			}
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].v > sorted[j].v })
			fmt.Fprintf(&sb, "\n*%s breakdown by %s:*\n```\n", p.Start, r.GroupBy)
			limit := 10
			if len(sorted) < limit {
				limit = len(sorted)
			}
			for _, kv := range sorted[:limit] {
				fmt.Fprintf(&sb, "%-50s %12s\n", truncate(kv.k, 50), money(kv.v, p.Unit))
			}
			if len(sorted) > limit {
				var rest float64
				for _, kv := range sorted[limit:] {
					rest += kv.v
				}
				fmt.Fprintf(&sb, "%-50s %12s (%d more)\n", "(other)", money(rest, p.Unit), len(sorted)-limit)
			}
			sb.WriteString("```\n")
		}
	}
	return sb.String()
}

// FormatForecast returns a Slack-friendly rendering of a ForecastResult.
func FormatForecast(r *ForecastResult) string {
	if r == nil || len(r.Periods) == 0 {
		return "No forecast data returned."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*AWS %s forecast — %s (%s → %s)*\n", strings.ToLower(r.Granularity), r.Metric, r.Start, r.End)
	fmt.Fprintf(&sb, "Total projected: *%s*\n", money(r.Total, r.Unit))
	sb.WriteString("```\n")
	fmt.Fprintf(&sb, "%-12s  %12s\n", "period", r.Unit)
	sb.WriteString(strings.Repeat("─", 28) + "\n")
	for _, p := range r.Periods {
		fmt.Fprintf(&sb, "%-12s  %12s\n", p.Start, money(p.Total, p.Unit))
	}
	sb.WriteString("```\n")
	return sb.String()
}

// FormatDimensionValues returns a Slack-friendly list of dimension values.
// Long lists are capped at 100 rows with a truncation note.
func FormatDimensionValues(r *DimensionValuesResult) string {
	if r == nil || len(r.Values) == 0 {
		return fmt.Sprintf("No values found for dimension %s in %s → %s.", r.Dimension, r.Start, r.End)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*AWS %s values (%s → %s):*\n```\n", r.Dimension, r.Start, r.End)
	limit := 100
	if len(r.Values) < limit {
		limit = len(r.Values)
	}
	for _, v := range r.Values[:limit] {
		sb.WriteString(v)
		sb.WriteString("\n")
	}
	sb.WriteString("```\n")
	if len(r.Values) > limit {
		fmt.Fprintf(&sb, "_(showing first %d of %d — narrow with `search`)_\n", limit, len(r.Values))
	}
	return sb.String()
}

// money formats a Cost Explorer amount as "$1,234.56" for USD, or
// "1,234.56 EUR" for anything else. Unit is "USD" for billed accounts and
// may be empty for forecast/metric calls, in which case we assume USD.
func money(v float64, unit string) string {
	if unit == "" || unit == "USD" {
		return "$" + commify(v)
	}
	return commify(v) + " " + unit
}

// commify returns a number with thousands separators and two decimals.
func commify(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	dot := strings.IndexByte(s, '.')
	intPart, fracPart := s[:dot], s[dot:]
	// Insert commas right-to-left.
	n := len(intPart)
	if n <= 3 {
		if neg {
			return "-" + intPart + fracPart
		}
		return intPart + fracPart
	}
	var b strings.Builder
	rem := n % 3
	if rem > 0 {
		b.WriteString(intPart[:rem])
		if n > rem {
			b.WriteByte(',')
		}
	}
	for i := rem; i < n; i += 3 {
		b.WriteString(intPart[i : i+3])
		if i+3 < n {
			b.WriteByte(',')
		}
	}
	b.WriteString(fracPart)
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// firstUnit returns the first non-empty unit across periods, defaulting to USD.
func firstUnit(periods []CostPeriod) string {
	for _, p := range periods {
		if p.Unit != "" {
			return p.Unit
		}
	}
	return "USD"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return s[:n-1] + "…"
}
