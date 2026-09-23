package slack

import (
	"strings"
	"unicode/utf8"
)

// MaxMessageRunes keeps a post under the length at which Slack splits a
// message on its own, which it does without regard for open code blocks.
const MaxMessageRunes = 3500

const fence = "```"

// RenderPipeTables replaces every ```table fenced block, whose rows are cells
// separated by "|", with a plain code block of padded columns: the first
// column left-aligned, the rest right-aligned.
func RenderPipeTables(text string) string {
	if !strings.Contains(text, fence+"table") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != fence+"table" {
			out = append(out, lines[i])
			continue
		}
		end := i + 1
		for end < len(lines) && strings.TrimSpace(lines[end]) != fence {
			end++
		}
		if end == len(lines) {
			out = append(out, lines[i:]...)
			break
		}
		out = append(out, fence)
		out = append(out, alignRows(lines[i+1:end])...)
		out = append(out, fence)
		i = end
	}
	return strings.Join(out, "\n")
}

func alignRows(lines []string) []string {
	var rows [][]string
	var widths []int
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		t = strings.TrimSuffix(strings.TrimPrefix(t, "|"), "|")
		cells := strings.Split(t, "|")
		for j := range cells {
			cells[j] = strings.TrimSpace(cells[j])
		}
		if isSeparatorRow(cells) {
			continue
		}
		for j, c := range cells {
			if j == len(widths) {
				widths = append(widths, 0)
			}
			if n := utf8.RuneCountInString(c); n > widths[j] {
				widths[j] = n
			}
		}
		rows = append(rows, cells)
	}
	out := make([]string, 0, len(rows))
	for _, cells := range rows {
		var sb strings.Builder
		for j, w := range widths {
			c := ""
			if j < len(cells) {
				c = cells[j]
			}
			pad := strings.Repeat(" ", w-utf8.RuneCountInString(c))
			if j > 0 {
				sb.WriteString("  ")
				sb.WriteString(pad)
				sb.WriteString(c)
			} else {
				sb.WriteString(c)
				sb.WriteString(pad)
			}
		}
		out = append(out, strings.TrimRight(sb.String(), " "))
	}
	return out
}

func isSeparatorRow(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, "-: ") != "" {
			return false
		}
	}
	return true
}

// SplitForSlack breaks text into pieces of at most limit runes at line
// boundaries. A piece that ends inside a code block closes it, and the next
// piece reopens it.
func SplitForSlack(text string, limit int) []string {
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}
	// Room for a closing fence plus newline on a piece cut inside a block.
	budget := limit - len(fence) - 1
	var (
		pieces []string
		cur    []string
		size   int
		inCode bool
	)
	flush := func() {
		if len(cur) == 0 {
			return
		}
		piece := strings.Join(cur, "\n")
		if inCode {
			piece += "\n" + fence
		}
		pieces = append(pieces, piece)
		cur, size = nil, 0
		if inCode {
			cur, size = []string{fence}, len(fence)+1
		}
	}
	for _, line := range strings.Split(text, "\n") {
		for utf8.RuneCountInString(line) > budget-len(fence)-1 {
			head, rest := splitRunes(line, budget-len(fence)-1)
			flush()
			cur, size = append(cur, head), size+utf8.RuneCountInString(head)+1
			line = rest
		}
		n := utf8.RuneCountInString(line) + 1
		if size+n > budget {
			flush()
		}
		cur = append(cur, line)
		size += n
		if strings.Count(line, fence)%2 == 1 {
			inCode = !inCode
		}
	}
	if len(cur) > 0 {
		pieces = append(pieces, strings.Join(cur, "\n"))
	}
	return pieces
}

func splitRunes(s string, n int) (string, string) {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos], s[pos:]
		}
		i++
	}
	return s, ""
}
