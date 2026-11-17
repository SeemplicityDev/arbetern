package text

import (
	"math"
	"strconv"
	"strings"
)

// Commify formats v with two decimals and comma thousands separators, e.g. -1234.5 → "-1,234.50".
func Commify(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	s := strconv.FormatFloat(math.Abs(v), 'f', 2, 64)
	intPart, frac := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart, frac = s[:dot], s[dot:]
	}
	var b strings.Builder
	if v < 0 {
		b.WriteByte('-')
	}
	for i := range len(intPart) {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(intPart[i])
	}
	b.WriteString(frac)
	return b.String()
}
