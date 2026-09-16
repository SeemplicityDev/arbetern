package text

import (
	"html"
	"regexp"
	"strings"
)

var (
	htmlDropRE   = regexp.MustCompile(`(?is)<(script|style|head)\b[^>]*>.*?</(script|style|head)>`)
	htmlBlockRE  = regexp.MustCompile(`(?i)</?(p|div|br|li|ul|ol|h[1-6]|tr|table|blockquote|pre|section|article|hr)\b[^>]*>`)
	htmlTagRE    = regexp.MustCompile(`<[^>]*>`)
	blankLinesRE = regexp.MustCompile(`\n{3,}`)
	spaceRunRE   = regexp.MustCompile(`[ \t]{2,}`)
)

// StripHTML reduces an HTML fragment to readable plain text: block elements
// become line breaks, other tags are dropped and entities are decoded.
func StripHTML(s string) string {
	if !strings.Contains(s, "<") {
		return strings.TrimSpace(html.UnescapeString(s))
	}
	s = htmlDropRE.ReplaceAllString(s, "")
	s = htmlBlockRE.ReplaceAllString(s, "\n")
	s = htmlTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spaceRunRE.ReplaceAllString(s, " ")
	s = blankLinesRE.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
