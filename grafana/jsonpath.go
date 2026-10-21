package grafana

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// evalJSONPath evaluates a JSONPath expression against decoded JSON. A path
// of plain keys and indexes returns one value; wildcards, recursive descent,
// unions, slices and filters return a list of matches.
func evalJSONPath(root any, path string) (any, error) {
	path = strings.TrimSpace(path)
	switch {
	case path == "" || path == "$":
		return root, nil
	case strings.HasPrefix(path, "$"):
		path = path[1:]
	case strings.HasPrefix(path, ".") || strings.HasPrefix(path, "["):
	default:
		path = "." + path
	}
	segs, err := parseJSONPath(path)
	if err != nil {
		return nil, err
	}
	nodes := []any{root}
	definite := true
	for _, s := range segs {
		if s.recursive || !s.definite() {
			definite = false
		}
		var next []any
		for _, n := range nodes {
			targets := []any{n}
			if s.recursive {
				targets = descendants(n)
			}
			for _, t := range targets {
				matched, err := s.apply(t, definite)
				if err != nil {
					return nil, err
				}
				next = append(next, matched...)
			}
		}
		nodes = next
	}
	if definite {
		if len(nodes) == 0 {
			return nil, errors.New("the path matched nothing")
		}
		return nodes[0], nil
	}
	if nodes == nil {
		nodes = []any{}
	}
	return nodes, nil
}

type jsonPathSegment struct {
	recursive bool
	wildcard  bool
	names     []string
	indices   []int
	slice     *[3]*int
	filter    *jsonPathFilter
}

func (s jsonPathSegment) definite() bool {
	return !s.wildcard && s.slice == nil && s.filter == nil && len(s.names)+len(s.indices) == 1
}

func (s jsonPathSegment) apply(n any, strict bool) ([]any, error) {
	var out []any
	switch v := n.(type) {
	case map[string]any:
		switch {
		case s.wildcard:
			for _, k := range sortedKeys(v) {
				out = append(out, v[k])
			}
		case s.filter != nil:
			for _, k := range sortedKeys(v) {
				if s.filter.match(v[k]) {
					out = append(out, v[k])
				}
			}
		default:
			for _, name := range s.names {
				if x, ok := v[name]; ok {
					out = append(out, x)
				} else if strict {
					return nil, fmt.Errorf("no key %q here (keys: %s)", name, strings.Join(firstKeys(v, 15), ", "))
				}
			}
		}
	case []any:
		switch {
		case s.wildcard:
			out = append(out, v...)
		case s.filter != nil:
			for _, x := range v {
				if s.filter.match(x) {
					out = append(out, x)
				}
			}
		case s.slice != nil:
			start, end, step := 0, len(v), 1
			if s.slice[2] != nil && *s.slice[2] != 0 {
				step = *s.slice[2]
			}
			if step < 0 {
				return nil, errors.New("negative slice steps are not supported")
			}
			if s.slice[0] != nil {
				start = normIndex(*s.slice[0], len(v))
			}
			if s.slice[1] != nil {
				end = normIndex(*s.slice[1], len(v))
			}
			for i := start; i < end && i < len(v); i += step {
				out = append(out, v[i])
			}
		default:
			for _, idx := range s.indices {
				i := idx
				if i < 0 {
					i += len(v)
				}
				if i >= 0 && i < len(v) {
					out = append(out, v[i])
				} else if strict {
					return nil, fmt.Errorf("index %d is out of range (length %d)", idx, len(v))
				}
			}
			if strict && len(s.names) > 0 {
				return nil, fmt.Errorf("key %q was applied to a list; use an index or [*]", s.names[0])
			}
		}
	default:
		if strict {
			return nil, fmt.Errorf("cannot select into a %T value", n)
		}
	}
	return out, nil
}

func normIndex(i, n int) int {
	if i < 0 {
		i += n
	}
	if i < 0 {
		return 0
	}
	if i > n {
		return n
	}
	return i
}

func descendants(n any) []any {
	out := []any{n}
	switch v := n.(type) {
	case map[string]any:
		for _, k := range sortedKeys(v) {
			out = append(out, descendants(v[k])...)
		}
	case []any:
		for _, x := range v {
			out = append(out, descendants(x)...)
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstKeys(m map[string]any, n int) []string {
	keys := sortedKeys(m)
	if len(keys) > n {
		keys = append(keys[:n], "…")
	}
	return keys
}

func parseJSONPath(p string) ([]jsonPathSegment, error) {
	var segs []jsonPathSegment
	for i := 0; i < len(p); {
		var seg jsonPathSegment
		switch {
		case strings.HasPrefix(p[i:], ".."):
			seg.recursive = true
			i += 2
		case p[i] == '.':
			i++
		case p[i] == '[':
		default:
			return nil, fmt.Errorf("unexpected %q at position %d", p[i], i)
		}
		if i >= len(p) {
			return nil, errors.New("the path ends after a dot")
		}
		switch p[i] {
		case '*':
			seg.wildcard = true
			i++
		case '[':
			end := matchingBracket(p, i)
			if end < 0 {
				return nil, fmt.Errorf("unclosed [ at position %d", i)
			}
			if err := parseBracket(strings.TrimSpace(p[i+1:end]), &seg); err != nil {
				return nil, err
			}
			i = end + 1
		default:
			j := i
			for j < len(p) && p[j] != '.' && p[j] != '[' {
				j++
			}
			seg.names = []string{p[i:j]}
			i = j
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

// matchingBracket finds the ] closing the [ at open, skipping quoted text and
// nested brackets inside filters.
func matchingBracket(p string, open int) int {
	depth := 0
	var quote byte
	for i := open; i < len(p); i++ {
		c := p[i]
		switch {
		case quote != 0:
			switch c {
			case '\\':
				i++
			case quote:
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func parseBracket(in string, seg *jsonPathSegment) error {
	switch {
	case in == "*":
		seg.wildcard = true
		return nil
	case strings.HasPrefix(in, "?"):
		f, err := parseFilter(strings.TrimSpace(in[1:]))
		if err != nil {
			return err
		}
		seg.filter = f
		return nil
	case strings.Contains(in, ":") && !strings.ContainsAny(in, `'"`):
		parts := strings.Split(in, ":")
		if len(parts) > 3 {
			return fmt.Errorf("bad slice [%s]", in)
		}
		var sl [3]*int
		for k, part := range parts {
			if part = strings.TrimSpace(part); part != "" {
				n, err := strconv.Atoi(part)
				if err != nil {
					return fmt.Errorf("bad slice [%s]", in)
				}
				sl[k] = &n
			}
		}
		seg.slice = &sl
		return nil
	}
	for _, part := range splitUnion(in) {
		part = strings.TrimSpace(part)
		if len(part) >= 2 && (part[0] == '\'' || part[0] == '"') && part[len(part)-1] == part[0] {
			seg.names = append(seg.names, part[1:len(part)-1])
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return fmt.Errorf("bad selector [%s]: quote keys, e.g. ['%s']", in, part)
		}
		seg.indices = append(seg.indices, n)
	}
	return nil
}

func splitUnion(in string) []string {
	var parts []string
	var quote byte
	last := 0
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ',':
			parts = append(parts, in[last:i])
			last = i + 1
		}
	}
	return append(parts, in[last:])
}

type jsonPathFilter struct {
	path  string
	op    string
	value any
	re    *regexp.Regexp
}

var filterOpRe = regexp.MustCompile(`\s*(==|!=|<=|>=|=~|<|>)\s*`)

func parseFilter(in string) (*jsonPathFilter, error) {
	if !strings.HasPrefix(in, "(") || !strings.HasSuffix(in, ")") {
		return nil, fmt.Errorf("filters look like [?(@.type == 'timeseries')], got [?%s]", in)
	}
	in = strings.TrimSpace(in[1 : len(in)-1])
	f := &jsonPathFilter{}
	loc := filterOpRe.FindStringIndex(in)
	left := in
	if loc != nil {
		left = in[:loc[0]]
		f.op = strings.TrimSpace(in[loc[0]:loc[1]])
		right := strings.TrimSpace(in[loc[1]:])
		switch {
		case f.op == "=~":
			pat := strings.Trim(right, "/'\"")
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("bad filter regex %q: %v", pat, err)
			}
			f.re = re
		case len(right) >= 2 && (right[0] == '\'' || right[0] == '"') && right[len(right)-1] == right[0]:
			f.value = right[1 : len(right)-1]
		case right == "true" || right == "false":
			f.value = right == "true"
		case right == "null":
			f.value = nil
		default:
			n, err := strconv.ParseFloat(right, 64)
			if err != nil {
				return nil, fmt.Errorf("bad filter value %q", right)
			}
			f.value = n
		}
	}
	left = strings.TrimSpace(left)
	if !strings.HasPrefix(left, "@") {
		return nil, fmt.Errorf("filter paths start with @, got %q", left)
	}
	f.path = "$" + left[1:]
	return f, nil
}

func (f *jsonPathFilter) match(n any) bool {
	v, err := evalJSONPath(n, f.path)
	if err != nil {
		return false
	}
	switch f.op {
	case "":
		return v != nil && v != false
	case "=~":
		s, ok := v.(string)
		return ok && f.re.MatchString(s)
	case "==":
		return equalJSON(v, f.value)
	case "!=":
		return !equalJSON(v, f.value)
	}
	a, aok := v.(float64)
	b, bok := f.value.(float64)
	if !aok || !bok {
		return false
	}
	switch f.op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func equalJSON(a, b any) bool {
	if af, ok := a.(float64); ok {
		bf, ok := b.(float64)
		return ok && af == bf
	}
	return a == b
}
