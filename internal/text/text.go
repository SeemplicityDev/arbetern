// Package text holds small string helpers shared across packages.
package text

import "unicode/utf8"

// Ellipsis is appended by Truncate when it shortens a string.
const Ellipsis = "…"

// Truncate shortens s to at most max bytes, appending Ellipsis when it cuts.
// The cut lands on a rune boundary, so the result is always valid UTF-8 even
// when max falls inside a multi-byte sequence.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	if max <= len(Ellipsis) {
		return trimToRune(s, max)
	}
	return trimToRune(s, max-len(Ellipsis)) + Ellipsis
}

// TruncatePlain shortens s to at most max bytes on a rune boundary and adds
// no marker. Use it where the caller supplies its own suffix.
func TruncatePlain(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return trimToRune(s, max)
}

func trimToRune(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
