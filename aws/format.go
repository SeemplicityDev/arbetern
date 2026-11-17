package aws

import (
	"fmt"
	"strings"

	"github.com/justmike1/arbetern/internal/text"
)

// FormatCostAndUsage returns a Slack-friendly rendering of a CostAndUsageResult.
// It's intentionally compact: a header line, then one line per period, with
// grouped periods unfolded into a sorted list below each period's total.
func FormatCostAndUsage(r *CostAndUsageResult) string {
	if r == nil || len(r.Periods) == 0 {
		return "No cost data returned for the requested window."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*AWS %s cost — %s (%s → %s)*\n", strings.ToLower(r.Granularity), r.Metric, r.Start, r.End)
	if r.GroupBy != "" {
		fmt.Fprintf(&sb, "_Grouped by %s_\n", r.GroupBy)
	}
	sb.WriteString(periodTable(firstUnit(r.Periods), r.Periods))
	if r.GroupBy != "" {
		// Show groups under each period (top 10 by cost, since some dimensions
		// like USAGE_TYPE can return hundreds of rows that would bury Slack).
		for _, p := range r.Periods {
			if len(p.Groups) == 0 {
				continue
			}
			fmt.Fprintf(&sb, "\n*%s breakdown by %s:*\n", p.Start, r.GroupBy)
			rows := text.RankedRows(p.Groups, r.GroupLimit, func(v float64) string { return money(v, p.Unit) })
			sb.WriteString(text.Table{Rows: rows, Align: amountAlign}.String())
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
		return "$" + text.Commify(v)
	}
	return text.Commify(v) + " " + unit
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
