package text

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Align places a cell within its column; the zero value is AlignLeft.
type Align int

// Column alignments.
const (
	AlignLeft Align = iota
	AlignRight
)

// Table defaults, used when the matching Table field is zero.
const (
	DefaultMaxPad   = 80
	DefaultMaxBytes = 64 << 10
)

// HintSQL is the omitted-rows advice for SQL result tables.
const HintSQL = "aggregate in SQL or select fewer columns"

// Table renders tool output as an aligned text table that never cuts a cell.
type Table struct {
	Headers  []string   // nil renders a headerless grid
	Rows     [][]string // short rows render their missing cells empty
	Align    []Align    // per column; columns past the slice align left
	MaxPad   int        // widest a column is padded; a longer cell is written whole and pushes the rest of its row right
	MaxBytes int        // once the table reaches this size, the remaining rows are counted in a note instead of shown
	Hint     string     // advice appended to that note
	NoFence  bool       // render without the surrounding code fence
}

// String renders the table, followed by a note when MaxBytes leaves rows out.
func (t Table) String() string {
	cols := len(t.Headers)
	for _, r := range t.Rows {
		cols = max(cols, len(r))
	}
	if cols == 0 {
		return ""
	}
	maxPad, maxBytes := t.MaxPad, t.MaxBytes
	if maxPad <= 0 {
		maxPad = DefaultMaxPad
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	widths := make([]int, cols)
	measure := func(cells []string) {
		for i, c := range cells {
			widths[i] = max(widths[i], min(utf8.RuneCountInString(c), maxPad))
		}
	}
	measure(t.Headers)
	for _, r := range t.Rows {
		measure(r)
	}

	var sb strings.Builder
	if !t.NoFence {
		sb.WriteString("```\n")
	}
	if len(t.Headers) > 0 {
		t.writeRow(&sb, t.Headers, widths)
		rule := make([]string, cols)
		for i, w := range widths {
			rule[i] = strings.Repeat("─", w)
		}
		t.writeRow(&sb, rule, widths)
	}
	shown := 0
	for _, r := range t.Rows {
		if sb.Len() >= maxBytes {
			break
		}
		t.writeRow(&sb, r, widths)
		shown++
	}
	if !t.NoFence {
		sb.WriteString("```\n")
	}
	if shown < len(t.Rows) {
		fmt.Fprintf(&sb, "_(showing first %d of %d rows", shown, len(t.Rows))
		if t.Hint != "" {
			sb.WriteString(" — " + t.Hint)
		}
		sb.WriteString(")_\n")
	}
	return sb.String()
}

// RankedRows turns label→amount pairs into rows, largest first, folding everything past limit (when > 0) into one "(other)" row.
func RankedRows(amounts map[string]float64, limit int, format func(float64) string) [][]string {
	labels := make([]string, 0, len(amounts))
	for k := range amounts {
		labels = append(labels, k)
	}
	slices.SortFunc(labels, func(a, b string) int {
		return cmp.Or(cmp.Compare(amounts[b], amounts[a]), strings.Compare(a, b))
	})
	if limit <= 0 || limit > len(labels) {
		limit = len(labels)
	}
	rows := make([][]string, 0, limit+1)
	for _, k := range labels[:limit] {
		rows = append(rows, []string{k, format(amounts[k])})
	}
	if rest := labels[limit:]; len(rest) > 0 {
		var sum float64
		for _, k := range rest {
			sum += amounts[k]
		}
		rows = append(rows, []string{"(other)", format(sum), fmt.Sprintf("(%d more)", len(rest))})
	}
	return rows
}

func (t Table) writeRow(sb *strings.Builder, cells []string, widths []int) {
	last := len(cells) - 1
	for last >= 0 && cells[last] == "" {
		last--
	}
	for i := 0; i <= last; i++ {
		if i > 0 {
			sb.WriteString("  ")
		}
		pad := strings.Repeat(" ", max(0, widths[i]-utf8.RuneCountInString(cells[i])))
		switch {
		case i < len(t.Align) && t.Align[i] == AlignRight:
			sb.WriteString(pad)
			sb.WriteString(cells[i])
		case i == last:
			sb.WriteString(cells[i])
		default:
			sb.WriteString(cells[i])
			sb.WriteString(pad)
		}
	}
	sb.WriteByte('\n')
}
