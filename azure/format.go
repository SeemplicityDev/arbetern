package azure

import (
	"fmt"
	"sort"
	"strings"
)

// FormatCostAndUsage returns a Slack-friendly rendering of a CostAndUsageResult.
// Mirrors aws.FormatCostAndUsage so the tool output looks consistent across
// cloud providers.
func FormatCostAndUsage(r *CostAndUsageResult) string {
	if r == nil || len(r.Periods) == 0 {
		return "No Azure cost data returned for the requested window."
	}
	var sb strings.Builder
	unit := firstUnit(r.Periods)
	fmt.Fprintf(&sb, "*Azure %s cost — %s (%s → %s)*\n", strings.ToLower(r.Granularity), r.Metric, r.Start, r.End)
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
		return "No Azure forecast data returned."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Azure %s forecast — %s (%s → %s)*\n", strings.ToLower(r.Granularity), r.Metric, r.Start, r.End)
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
func FormatDimensionValues(r *DimensionValuesResult) string {
	if r == nil || len(r.Values) == 0 {
		return fmt.Sprintf("No Azure values found for dimension %s.", func() string {
			if r == nil {
				return "(unknown)"
			}
			return r.Dimension
		}())
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Azure %s values:*\n```\n", r.Dimension)
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

func money(v float64, unit string) string {
	if unit == "" || unit == "USD" {
		return "$" + commify(v)
	}
	return commify(v) + " " + unit
}

func commify(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	dot := strings.IndexByte(s, '.')
	intPart, fracPart := s[:dot], s[dot:]
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

func firstUnit(periods []CostPeriod) string {
	for _, p := range periods {
		if p.Unit != "" {
			return p.Unit
		}
	}
	return "USD"
}
