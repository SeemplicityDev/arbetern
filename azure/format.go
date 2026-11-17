package azure

import (
	"fmt"
	"strings"

	"github.com/justmike1/arbetern/internal/text"
)

// FormatCostAndUsage returns a Slack-friendly rendering of a CostAndUsageResult.
// Mirrors aws.FormatCostAndUsage so the tool output looks consistent across
// cloud providers.
func FormatCostAndUsage(r *CostAndUsageResult) string {
	if r == nil || len(r.Periods) == 0 {
		return "No Azure cost data returned for the requested window."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Azure %s cost — %s (%s → %s)*\n", strings.ToLower(r.Granularity), r.Metric, r.Start, r.End)
	if r.GroupBy != "" {
		fmt.Fprintf(&sb, "_Grouped by %s_\n", r.GroupBy)
	}
	sb.WriteString(periodTable(firstUnit(r.Periods), r.Periods))
	if r.GroupBy != "" {
		for _, p := range r.Periods {
			if len(p.Groups) == 0 {
				continue
			}
			fmt.Fprintf(&sb, "\n*%s breakdown by %s:*\n", p.Start, r.GroupBy)
			rows := text.RankedRows(p.Groups, 10, func(v float64) string { return money(v, p.Unit) })
			sb.WriteString(text.Table{Rows: rows, Align: amountAlign}.String())
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
	sb.WriteString(periodTable(r.Unit, r.Periods))
	return sb.String()
}

var amountAlign = []text.Align{text.AlignLeft, text.AlignRight}

func periodTable(unit string, periods []CostPeriod) string {
	rows := make([][]string, 0, len(periods))
	for _, p := range periods {
		rows = append(rows, []string{p.Start, money(p.Total, p.Unit)})
	}
	return text.Table{Headers: []string{"period", unit}, Rows: rows, Align: amountAlign}.String()
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
		return "$" + text.Commify(v)
	}
	return text.Commify(v) + " " + unit
}

func firstUnit(periods []CostPeriod) string {
	for _, p := range periods {
		if p.Unit != "" {
			return p.Unit
		}
	}
	return "USD"
}
