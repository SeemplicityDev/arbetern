// Package redact scrubs secrets and personal data from free text.
package redact

import (
	"net"
	"regexp"
	"slices"
	"strings"
)

const (
	secretKey = `[A-Za-z0-9_.-]*(?:pass(?:word|wd)?|secret|token|api[ _-]?key|private[ _-]?key|credential|cookie|session)[A-Za-z0-9_.-]*`
	bareValue = "[^\\s\"'`&,;(){}\\[\\]\\\\]+"
)

// A rule only runs when the lower-cased input contains one of its needles; every match contains one.
var rules = []struct {
	needles []string
	re      *regexp.Regexp
	repl    string
}{
	{[]string{"private key"}, regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----|\z)`), "<redacted-private-key>"},
	{[]string{"sk-ant-"}, regexp.MustCompile(`(^|[^A-Za-z0-9])sk-ant-[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*`), "${1}sk-ant-<redacted>"},
	{[]string{"eyj"}, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`), "<redacted-jwt>"},
	{[]string{"xox", "xapp-"}, regexp.MustCompile(`\b(xox[abeposr]|xapp)-[A-Za-z0-9-]{10,}`), "${1}-<redacted>"},
	{[]string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}, regexp.MustCompile(`\b(gh[pousr]_)[A-Za-z0-9]{30,}`), "${1}<redacted>"},
	{[]string{"github_pat_"}, regexp.MustCompile(`\b(github_pat_)[A-Za-z0-9_]{22,}`), "${1}<redacted>"},
	{[]string{"akia", "asia"}, regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`), "${1}<redacted>"},
	{[]string{"@"}, regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*:(?:\\?/){2})[^\s/?#"'<>\\]+@`), "${1}<redacted>@"},
	{[]string{"bearer"}, regexp.MustCompile(`(?i)\b(bearer[ \t]+)[A-Za-z0-9._~+/-]{16,}=*`), "${1}<redacted>"},
}

// Each header rule captures the prefix to keep, then the credential.
var (
	authRe   = regexp.MustCompile(`(?i)(authorization\\?["']?[ \t]*[:=][ \t]*\\?["']?(?:(?:bearer|basic|token|digest|negotiate)[ \t]+)?)(` + bareValue + `)`)
	cookieRe = regexp.MustCompile(`(?i)\b((?:set-)?cookie:[ \t]*)([^\s"'][^\r\n"']*)`)
	// Go prints map[string][]string (http.Header, url.Values) as map[Key:[v1 v2]].
	listRe = regexp.MustCompile(`(?i)(\b(?:` + secretKey + `|authorization):\[)([^\]\r\n]*)\]`)
)

var (
	pairNeedles = []string{"pass", "secret", "token", "api", "private", "credential", "cookie", "session"}
	// Groups: key, closing quote, space, operator, space, value.
	pairRe        = regexp.MustCompile(`(?i)(` + secretKey + `)(\\?["']?)([ \t]*)(=>?|:)([ \t]*)("[^"\r\n]+"?|'[^'\r\n]+'?|\\"[^"\\\r\n]+(?:\\")?|` + bareValue + `)`)
	harmlessRe    = regexp.MustCompile(`^(?i:[+-]?\d{1,12}(?:\.\d+)?|true|false|null|nil|<nil>|none|undefined)$`)
	exceptionRe   = regexp.MustCompile(`(?i)(?:error|exception|warning)$`)
	frameRefRe    = regexp.MustCompile(`^(?:\d{1,12}(?:[.:]\d{1,12})*(?::|:in)?|line)$`)
	placeholderRe = regexp.MustCompile(`<(?:redacted|email>|ip>|number>)`)
	emailRe       = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`)
	retinaRe      = regexp.MustCompile(`^\d+(?:\.\d+)?x\.`)
	ipv4Re        = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	longNumberRe  = regexp.MustCompile(`\b\d{13,19}\b`)
)

// Text returns s with credentials, tokens and personal data replaced by placeholders.
func Text(s string) string {
	lower := strings.ToLower(s)
	has := func(needles ...string) bool {
		return slices.ContainsFunc(needles, func(n string) bool { return strings.Contains(lower, n) })
	}
	for _, r := range rules {
		if has(r.needles...) {
			s = r.re.ReplaceAllString(s, r.repl)
		}
	}
	if has(pairNeedles...) || has("authorization") {
		s = redactHeader(s, listRe)
	}
	if has("authorization") {
		s = redactHeader(s, authRe)
	}
	if has("cookie") {
		s = redactHeader(s, cookieRe)
	}
	if has(pairNeedles...) {
		s = redactPairs(s)
	}
	if has("@") {
		s = emailRe.ReplaceAllStringFunc(s, func(m string) string {
			if retinaRe.MatchString(m[strings.IndexByte(m, '@')+1:]) {
				return m
			}
			return "<email>"
		})
	}
	s = ipv4Re.ReplaceAllStringFunc(s, func(m string) string {
		if ip := net.ParseIP(m); ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			return m
		}
		return "<ip>"
	})
	return longNumberRe.ReplaceAllString(s, "<number>")
}

func redactHeader(s string, re *regexp.Regexp) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		sm := re.FindStringSubmatch(m)
		if keepValue(sm[2]) {
			return m
		}
		return sm[1] + "<redacted>" + m[len(sm[1])+len(sm[2]):]
	})
}

func keepValue(v string) bool {
	return harmlessRe.MatchString(strings.TrimSpace(v)) || placeholderRe.MatchString(v)
}

// redactPairs resumes scanning at the value of a pair it keeps, so a pair nested in that value is still found.
func redactPairs(s string) string {
	var b strings.Builder
	done, pos := 0, 0
	for pos < len(s) {
		m := pairRe.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		for i := range m {
			if m[i] >= 0 {
				m[i] += pos
			}
		}
		valStart, valEnd := m[12], m[13]
		if keepPair(s, m) {
			pos = valStart
			continue
		}
		open, _, closing := splitQuotes(s[valStart:valEnd])
		b.WriteString(s[done:valStart])
		b.WriteString(open + "<redacted>" + closing)
		done, pos = valEnd, valEnd
	}
	if done == 0 {
		return s
	}
	b.WriteString(s[done:])
	return b.String()
}

// keepPair leaves stack frames (File.js:42:13), code (token = f(x), token == x) and exception lines alone.
func keepPair(s string, m []int) bool {
	open, inner, _ := splitQuotes(s[m[12]:m[13]])
	if keepValue(inner) {
		return true
	}
	quotedKey := m[5] > m[4] && m[2] > 0 && (s[m[2]-1] == '"' || s[m[2]-1] == '\'')
	if quotedKey {
		return false
	}
	op := s[m[8]:m[9]]
	if op == ":" && exceptionRe.MatchString(s[m[2]:m[3]]) {
		return true
	}
	if open != "" {
		return false
	}
	spaceBefore, spaceAfter := m[7] > m[6], m[11] > m[10]
	if spaceBefore && strings.HasPrefix(inner, "=") {
		return true
	}
	if op == ":" {
		return !spaceAfter && frameRefRe.MatchString(inner)
	}
	rest := s[m[13]:]
	return spaceBefore && (strings.HasPrefix(rest, "(") || strings.HasPrefix(rest, "["))
}

func splitQuotes(v string) (open, inner, closing string) {
	for _, q := range [...]string{`\"`, `"`, `'`} {
		if rest, ok := strings.CutPrefix(v, q); ok {
			if body, ok := strings.CutSuffix(rest, q); ok {
				return q, body, q
			}
			return q, rest, ""
		}
	}
	return "", v, ""
}
