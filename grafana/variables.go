package grafana

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const allMarker = "$__all"

// templateVarRe is Grafana's own variable syntax: $name, [[name:format]] and
// ${name.field:format}.
var templateVarRe = regexp.MustCompile(`\$(\w+)|\[\[(\w+?)(?::(\w+))?\]\]|\$\{(\w+)(?:\.([^:^\}]+))?(?::([^\}]+))?\}`)

// scope resolves variables the way the dashboard would with its saved
// selections, overridden by the caller.
type scope struct {
	vars     map[string]*Variable
	builtins map[string]string
	from, to time.Time
	// kind is the datasource family, which decides how an unformatted
	// multi-value variable is rendered.
	kind string
	used map[string]bool
	// missing names variables that are referenced but have no value Grafana
	// would not compute itself.
	missing map[string]bool
}

func newScope(d *Dashboard, overrides map[string]string, from, to time.Time, interval time.Duration) *scope {
	s := &scope{
		vars:    make(map[string]*Variable, len(d.Variables)),
		from:    from,
		to:      to,
		used:    map[string]bool{},
		missing: map[string]bool{},
	}
	for _, v := range d.Variables {
		s.vars[v.Name] = v
	}
	for name, value := range overrides {
		name = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(name, "var-"), "$"))
		if name == "" {
			continue
		}
		base := s.vars[name]
		ov := &Variable{Name: name, Type: "override"}
		if base != nil {
			cp := *base
			ov = &cp
		}
		applyOverride(ov, value)
		s.vars[name] = ov
	}
	rng := to.Sub(from)
	rangeSec := int64(rng.Round(time.Second) / time.Second)
	rate := interval + 15*time.Second
	if rate < time.Minute {
		rate = time.Minute
	}
	s.builtins = map[string]string{
		"__from":          strconv.FormatInt(from.UnixMilli(), 10),
		"__to":            strconv.FormatInt(to.UnixMilli(), 10),
		"__interval":      intervalText(interval),
		"__interval_ms":   strconv.FormatInt(interval.Milliseconds(), 10),
		"__range":         strconv.FormatInt(rangeSec, 10) + "s",
		"__range_s":       strconv.FormatInt(rangeSec, 10),
		"__range_ms":      strconv.FormatInt(rng.Milliseconds(), 10),
		"__rate_interval": intervalText(rate),
		"__dashboard":     d.Title,
	}
	return s
}

func applyOverride(v *Variable, value string) {
	value = strings.TrimSpace(value)
	v.IsAll = false
	if value == allMarker || (strings.EqualFold(value, "all") && v.IncludeAll) {
		v.IsAll, v.Values = true, nil
		return
	}
	if v.Multi || v.IncludeAll {
		var vals []string
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				vals = append(vals, part)
			}
		}
		v.Values = vals
		return
	}
	v.Values = []string{value}
}

// interpolate replaces every variable Grafana would; unknown names stay as written.
func (s *scope) interpolate(text string) string {
	return templateVarRe.ReplaceAllStringFunc(text, func(match string) string {
		m := templateVarRe.FindStringSubmatch(match)
		name := m[1]
		format := ""
		switch {
		case m[2] != "":
			name, format = m[2], m[3]
		case m[4] != "":
			name, format = m[4], m[6]
		}
		if out, ok := s.value(name, format); ok {
			return out
		}
		return match
	})
}

func (s *scope) value(name, format string) (string, bool) {
	if b, ok := s.builtins[name]; ok {
		if (name == "__from" || name == "__to") && strings.HasPrefix(format, "date") {
			return s.dateFormat(name, format), true
		}
		return b, true
	}
	v := s.vars[name]
	if v == nil || v.Type == "adhoc" {
		return "", false
	}
	s.used[name] = true
	values, multi := v.Values, v.Multi
	if v.IsAll {
		switch {
		case v.AllValue != "" && format == "":
			return v.AllValue, true
		case v.AllValue != "":
			values, multi = []string{v.AllValue}, false
		case len(v.Options) > 0:
			values, multi = v.Options, true
		default:
			s.missing[name] = true
			return "", false
		}
	}
	if v.Type == "interval" && len(values) > 0 && strings.HasPrefix(values[0], "$__auto") {
		values = []string{s.builtins["__interval"]}
	}
	if len(values) == 0 {
		if v.Type == "query" {
			s.missing[name] = true
			return "", false
		}
		return "", true
	}
	return formatValues(name, values, multi, format, s.kind), true
}

func (s *scope) dateFormat(name, format string) string {
	t := s.from
	if name == "__to" {
		t = s.to
	}
	switch strings.TrimPrefix(strings.TrimPrefix(format, "date"), ":") {
	case "seconds":
		return strconv.FormatInt(t.Unix(), 10)
	default:
		return t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
}

// selectionIsAllOrEmpty mirrors the ClickHouse plugin's $__conditionalAll test.
func (s *scope) selectionIsAllOrEmpty(name string) bool {
	v := s.vars[name]
	if v == nil {
		return false
	}
	return v.IsAll || len(v.Values) == 0 || (len(v.Values) == 1 && v.Values[0] == "")
}

// summary lists the variables the interpolation touched, as name=value.
func (s *scope) summary() []string {
	names := make([]string, 0, len(s.used))
	for n := range s.used {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		v := s.vars[n]
		if v == nil {
			continue
		}
		val := strings.Join(v.Values, ",")
		if v.IsAll {
			val = "All"
		}
		out = append(out, n+"="+val)
	}
	return out
}

func (s *scope) missingNames() []string {
	names := make([]string, 0, len(s.missing))
	for n := range s.missing {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// formatValues renders a selection with a named format, or with the
// datasource's own rule when none is given.
func formatValues(name string, vals []string, multi bool, format, kind string) string {
	if i := strings.IndexByte(format, ':'); i >= 0 {
		format = format[:i]
	}
	quoteEach := func(q, esc string) string {
		out := make([]string, len(vals))
		for i, v := range vals {
			out[i] = q + strings.ReplaceAll(v, q, esc) + q
		}
		return strings.Join(out, ",")
	}
	switch format {
	case "":
		return defaultFormat(vals, multi, kind)
	case "singlequote":
		return quoteEach("'", `\'`)
	case "doublequote":
		return quoteEach(`"`, `\"`)
	case "sqlstring":
		return quoteEach("'", "''")
	case "pipe":
		return strings.Join(vals, "|")
	case "glob":
		if len(vals) > 1 {
			return "{" + strings.Join(vals, ",") + "}"
		}
		return vals[0]
	case "regex":
		esc := make([]string, len(vals))
		for i, v := range vals {
			esc[i] = regexp.QuoteMeta(v)
		}
		if len(esc) > 1 {
			return "(" + strings.Join(esc, "|") + ")"
		}
		return esc[0]
	case "json":
		var b []byte
		if multi {
			b, _ = json.Marshal(vals)
		} else {
			b, _ = json.Marshal(vals[0])
		}
		return string(b)
	case "percentencode":
		if len(vals) > 1 {
			return url.QueryEscape("{" + strings.Join(vals, ",") + "}")
		}
		return url.QueryEscape(vals[0])
	case "queryparam":
		out := make([]string, len(vals))
		for i, v := range vals {
			out[i] = "var-" + name + "=" + url.QueryEscape(v)
		}
		return strings.Join(out, "&")
	case "lucene":
		if len(vals) == 1 {
			return luceneEscape(vals[0])
		}
		out := make([]string, len(vals))
		for i, v := range vals {
			out[i] = `"` + luceneEscape(v) + `"`
		}
		return "(" + strings.Join(out, " OR ") + ")"
	default:
		return strings.Join(vals, ",")
	}
}

func defaultFormat(vals []string, multi bool, kind string) string {
	switch kind {
	case "clickhouse":
		if multi {
			return "'" + strings.Join(vals, "','") + "'"
		}
		return vals[0]
	case "sql":
		if multi {
			out := make([]string, len(vals))
			for i, v := range vals {
				out[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
			}
			return strings.Join(out, ",")
		}
		return strings.ReplaceAll(vals[0], "'", "''")
	case "prometheus", "loki":
		if !multi {
			return vals[0]
		}
		esc := make([]string, len(vals))
		for i, v := range vals {
			esc[i] = regexp.QuoteMeta(v)
		}
		if len(esc) == 1 {
			return esc[0]
		}
		return "(" + strings.Join(esc, "|") + ")"
	}
	if multi && len(vals) > 1 {
		return "{" + strings.Join(vals, ",") + "}"
	}
	return vals[0]
}

func luceneEscape(v string) string {
	var sb strings.Builder
	for _, r := range v {
		if strings.ContainsRune(`+-&|!(){}[]^"~*?:\/ `, r) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

var conditionalVarRe = regexp.MustCompile(`^\$\{(\w+)\}|^\$(\w+)`)

// applyConditionalAll expands the ClickHouse plugin's frontend macro
// $__conditionalAll(condition, $var): the condition, or 1=1 when the variable
// is set to All or is empty. Like the plugin, it works from the last
// occurrence back so nested uses resolve inside out.
func (s *scope) applyConditionalAll(sql string) string {
	const macro = "$__conditionalAll("
	for guard := 0; guard < 500; guard++ {
		start := strings.LastIndex(sql, macro)
		if start < 0 {
			return sql
		}
		args, end, ok := macroArgs(sql, start+len(macro)-1)
		if !ok || len(args) != 2 {
			return sql
		}
		phrase := args[0]
		if m := conditionalVarRe.FindStringSubmatch(strings.TrimSpace(args[1])); m != nil {
			name := m[1] + m[2]
			if s.vars[name] != nil {
				s.used[name] = true
			}
			if s.selectionIsAllOrEmpty(name) {
				phrase = "1=1"
			}
		}
		sql = sql[:start] + phrase + sql[end+1:]
	}
	return sql
}

// macroArgs splits the arguments of the macro whose "(" is at open, counting
// brackets as the plugin does. end is the index of the closing ")".
func macroArgs(s string, open int) (args []string, end int, ok bool) {
	depth, last := 0, open+1
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return append(args, s[last:i]), i, true
			}
		case ',':
			if depth == 1 {
				args = append(args, s[last:i])
				last = i + 1
			}
		}
	}
	return nil, 0, false
}

// ── Intervals ──────────────────────────────────────────────────────────────

const defaultMaxDataPoints = 200

// panelInterval follows Grafana's calculation: the range over the data-point
// budget, rounded to a familiar step and raised to the panel's minimum.
func panelInterval(p *Panel, from, to time.Time, override string) time.Duration {
	if d, ok := parseInterval(override); ok {
		return d
	}
	points := defaultMaxDataPoints
	if p != nil && p.MaxDataPoints > 0 && p.MaxDataPoints < points {
		points = p.MaxDataPoints
	}
	interval := roundInterval(to.Sub(from) / time.Duration(points))
	if p != nil {
		if low, ok := parseInterval(p.Interval); ok && low > interval {
			interval = low
		}
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	return interval
}

var intervalRe = regexp.MustCompile(`^>?\s*(\d+)(ms|s|m|h|d|w|M|y)$`)

func parseInterval(s string) (time.Duration, bool) {
	m := intervalRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	if n <= 0 {
		return 0, false
	}
	unit := map[string]time.Duration{
		"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour,
		"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "M": 30 * 24 * time.Hour, "y": 365 * 24 * time.Hour,
	}[m[2]]
	return time.Duration(n) * unit, true
}

// roundInterval is Grafana's kbn.roundInterval table.
func roundInterval(d time.Duration) time.Duration {
	ms := d.Milliseconds()
	steps := []struct{ below, to int64 }{
		{15, 10}, {35, 20}, {75, 50}, {150, 100}, {350, 200}, {750, 500},
		{1500, 1000}, {3500, 2000}, {7500, 5000}, {12500, 10000}, {17500, 15000},
		{25000, 20000}, {45000, 30000}, {90000, 60000}, {210000, 120000},
		{450000, 300000}, {750000, 600000}, {1050000, 900000}, {1500000, 1200000},
		{2700000, 1800000}, {5400000, 3600000}, {9000000, 7200000}, {16200000, 10800000},
		{32400000, 21600000}, {86400000, 43200000}, {604800000, 86400000},
		{1814400000, 604800000}, {3628800000, 2592000000},
	}
	for _, s := range steps {
		if ms < s.below {
			return time.Duration(s.to) * time.Millisecond
		}
	}
	return 365 * 24 * time.Hour
}

// intervalText renders a duration the way Grafana's secondsToHms does.
func intervalText(d time.Duration) string {
	ms := d.Milliseconds()
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	s := ms / 1000
	switch {
	case s >= 31536000:
		return fmt.Sprintf("%dy", s/31536000)
	case s%31536000 >= 86400:
		return fmt.Sprintf("%dd", (s%31536000)/86400)
	case s%86400 >= 3600:
		return fmt.Sprintf("%dh", (s%86400)/3600)
	case s%3600 >= 60:
		return fmt.Sprintf("%dm", (s%3600)/60)
	}
	return fmt.Sprintf("%ds", s%60)
}
