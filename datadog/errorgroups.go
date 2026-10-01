package datadog

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/redact"
	"github.com/justmike1/arbetern/internal/text"
)

// ErrorSample is one log event kept as an example of an error group.
type ErrorSample struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Host      string    `json:"host,omitempty"`
	Message   string    `json:"message"`
	Stack     string    `json:"stack,omitempty"`
}

// ErrorGroup is the set of error logs that share one Fingerprint.
type ErrorGroup struct {
	Fingerprint string        `json:"fingerprint"`
	Service     string        `json:"service"`
	Kind        string        `json:"kind"`
	Pattern     string        `json:"pattern"`
	Frame       string        `json:"frame,omitempty"`
	Count       int           `json:"count"`
	FirstSeen   time.Time     `json:"first_seen"`
	LastSeen    time.Time     `json:"last_seen"`
	Samples     []ErrorSample `json:"samples,omitempty"` // the first event, then the most recent ones, oldest first
}

// ErrorGroupsResult is the outcome of one ErrorGroups scan.
type ErrorGroupsResult struct {
	Groups    []ErrorGroup // sorted by Count desc, then LastSeen desc
	Scanned   int          // events read
	Next      time.Time    // resume cursor: until when the window drained, else the last consumed event time
	Truncated bool         // maxEvents reached before the window drained
	Partial   bool         // Datadog reported meta.status "timeout" or warnings on any page
}

const (
	errorLogsPageLimit    = 1000
	minErrorLogsPageLimit = 50
	defaultErrorLogEvents = 5000
	maxErrorLogEvents     = 20000
	defaultErrorSamples   = 3
	maxErrorSamples       = 5
	sampleMessageMaxBytes = 2000
	sampleStackMaxBytes   = 8192
	patternMaxWords       = 24
	fingerprintVersion    = "v1"
	customFingerprintKind = "custom"
	pythonTracebackHeader = "Traceback (most recent call last):"
)

var (
	pythonFrameRe   = regexp.MustCompile(`^File "(.+?)", line \d+(?:, in (\S+))?`)
	atFrameRe       = regexp.MustCompile(`^[ \t]*at[ \t]+(?:async[ \t]+)?(.+?)[ \t]*$`)
	packagingDataRe = regexp.MustCompile(`[ \t]+~?\[[^\]]*\]$`)
	exceptionLineRe = regexp.MustCompile(`^(?:[ \t]*\|[ \t]?)?([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*)(?::(?:[ \t]|$)|[ \t]*$)`)
	lineNumberRe    = regexp.MustCompile(`:line \d+|:\d+`)
)

type logSearchRequest struct {
	Filter logSearchFilter `json:"filter"`
	Sort   string          `json:"sort"`
	Page   logSearchPage   `json:"page"`
}

type logSearchFilter struct {
	Query   string   `json:"query"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	Indexes []string `json:"indexes"`
}

type logSearchPage struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor,omitempty"`
}

// ErrorGroups reads the logs matching query between since and until, oldest first, and groups them by Fingerprint.
func (c *Client) ErrorGroups(ctx context.Context, query string, since, until time.Time, maxEvents, maxSamples int) (*ErrorGroupsResult, error) {
	if c == nil {
		return nil, errors.New("no Datadog client configured")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("query is required")
	}
	if since.IsZero() || !since.Before(until) {
		return nil, fmt.Errorf("since %s must be before until %s", since.Format(time.RFC3339Nano), until.Format(time.RFC3339Nano))
	}
	maxEvents = boundedOrDefault(maxEvents, defaultErrorLogEvents, maxErrorLogEvents)
	maxSamples = boundedOrDefault(maxSamples, defaultErrorSamples, maxErrorSamples)

	req := logSearchRequest{
		Filter: logSearchFilter{
			Query:   query,
			From:    strconv.FormatInt(since.UnixMilli(), 10),
			To:      strconv.FormatInt(until.UnixMilli(), 10),
			Indexes: []string{"*"},
		},
		Sort: "timestamp",
	}
	res := &ErrorGroupsResult{}
	groups := map[string]*groupBuilder{}
	var lastConsumed time.Time
	pageLimit := errorLogsPageLimit
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("searching error logs: %w", err)
		}
		req.Page.Limit = min(pageLimit, maxEvents-res.Scanned)
		var page LogSearchResponse
		if err := c.post(ctx, "/api/v2/logs/events/search", req, &page); err != nil {
			var syntaxErr *json.SyntaxError
			if !errors.As(err, &syntaxErr) {
				return nil, fmt.Errorf("searching error logs: %w", err)
			}
			// A page over maxResponseBody arrives cut short; retry the same cursor with fewer events.
			if req.Page.Limit > minErrorLogsPageLimit {
				pageLimit = max(req.Page.Limit/2, minErrorLogsPageLimit)
				continue
			}
			return nil, fmt.Errorf("searching error logs: decoding a page of %d events: %w", req.Page.Limit, syntaxErr)
		}
		if page.Meta.Status == "timeout" || len(page.Meta.Warnings) > 0 {
			res.Partial = true
		}
		consumed := page.Data[:min(len(page.Data), maxEvents-res.Scanned)]
		for _, entry := range consumed {
			if at := addErrorLog(groups, entry, maxSamples); at.After(lastConsumed) {
				lastConsumed = at
			}
		}
		res.Scanned += len(consumed)
		after := page.Meta.Page.After
		if after == "" && len(consumed) == len(page.Data) {
			res.Next = until
			break
		}
		if res.Scanned >= maxEvents {
			res.Truncated = true
			res.Next = resumeCursor(lastConsumed, since)
			break
		}
		if after == req.Page.Cursor {
			return nil, fmt.Errorf("searching error logs: page cursor did not advance after %d events", res.Scanned)
		}
		req.Page.Cursor = after
	}
	res.Groups = make([]ErrorGroup, 0, len(groups))
	for _, g := range groups {
		res.Groups = append(res.Groups, g.build())
	}
	slices.SortFunc(res.Groups, func(a, b ErrorGroup) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), b.LastSeen.Compare(a.LastSeen), strings.Compare(a.Fingerprint, b.Fingerprint))
	})
	return res, nil
}

// Fingerprint returns the stable 16-hex-character key of an error group.
func Fingerprint(service, kind, pattern, frame string) string {
	sum := sha256.Sum256([]byte(fingerprintVersion + "\x00" + service + "\x00" + kind + "\x00" + pattern + "\x00" + frame))
	return hex.EncodeToString(sum[:8])
}

func resumeCursor(lastConsumed, since time.Time) time.Time {
	// When every consumed event shares since's millisecond, resuming at it would reread the same events forever.
	if !lastConsumed.After(since) {
		return since.Add(time.Millisecond)
	}
	return lastConsumed
}

type groupBuilder struct {
	group      ErrorGroup
	first      ErrorSample
	recent     []ErrorSample
	keepRecent int
}

func addErrorLog(groups map[string]*groupBuilder, entry LogEntry, maxSamples int) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, entry.Attributes.Timestamp)
	e := parseErrorLog(entry.Attributes)
	fp := e.fingerprint()
	g := groups[fp]
	if g == nil {
		g = &groupBuilder{
			group:      ErrorGroup{Fingerprint: fp, Service: e.service, Kind: e.kind, Pattern: e.pattern, Frame: e.frame},
			keepRecent: maxSamples - 1,
		}
		groups[fp] = g
	}
	g.add(at, ErrorSample{
		ID:        entry.ID,
		Timestamp: at,
		Host:      entry.Attributes.Host,
		Message:   text.Truncate(e.message, sampleMessageMaxBytes),
		Stack:     text.TruncateStart(e.stack, sampleStackMaxBytes),
	})
	return at
}

func (g *groupBuilder) add(at time.Time, sample ErrorSample) {
	g.group.Count++
	if !at.IsZero() {
		if g.group.FirstSeen.IsZero() || at.Before(g.group.FirstSeen) {
			g.group.FirstSeen = at
		}
		if at.After(g.group.LastSeen) {
			g.group.LastSeen = at
		}
	}
	switch {
	case g.group.Count == 1:
		g.first = sample
	case len(g.recent) < g.keepRecent:
		g.recent = append(g.recent, sample)
	case g.keepRecent > 0:
		copy(g.recent, g.recent[1:])
		g.recent[len(g.recent)-1] = sample
	}
}

func (g *groupBuilder) build() ErrorGroup {
	out := g.group
	out.Samples = append([]ErrorSample{g.first}, g.recent...)
	return out
}

type errorLog struct {
	service, kind, pattern, frame, customFingerprint string
	message, stack                                   string
}

func parseErrorLog(attrs LogAttributes) errorLog {
	errMessage := attrString(attrs.Attributes, "error.message")
	stack := attrString(attrs.Attributes, "error.stack")
	if stack == "" {
		stack = cmp.Or(pythonTraceback(attrs.Message), pythonTraceback(errMessage))
	}
	stackKind, frame, exceptionMessage := parseStack(stack)
	return errorLog{
		service:           strings.TrimSpace(attrs.Service),
		kind:              cmp.Or(attrString(attrs.Attributes, "error.kind"), stackKind),
		pattern:           messagePattern(patternLine(errMessage, attrs.Message, exceptionMessage)),
		frame:             frame,
		customFingerprint: attrString(attrs.Attributes, "error.fingerprint"),
		message:           sampleMessage(attrs.Message, errMessage),
		stack:             stack,
	}
}

func (e errorLog) fingerprint() string {
	if e.customFingerprint != "" {
		return Fingerprint(e.service, customFingerprintKind, e.customFingerprint, "")
	}
	return Fingerprint(e.service, e.kind, e.pattern, e.frame)
}

func parseStack(stack string) (kind, frame, message string) {
	if file, fn, rest, ok := lastPythonFrame(stack); ok {
		kind, message = firstExceptionLine(rest)
		return kind, frameLabel(basename(file), fn), message
	}
	for line := range strings.Lines(stack) {
		line = strings.TrimRight(line, "\r\n")
		if m := atFrameRe.FindStringSubmatch(line); m != nil {
			return kind, atFrameLabel(m[1]), message
		}
		if kind == "" {
			kind, message, _ = exceptionLine(line)
		}
	}
	return kind, "", message
}

func lastPythonFrame(stack string) (file, fn, rest string, ok bool) {
	for end := len(stack); end > 0; {
		i := strings.LastIndex(stack[:end], `File "`)
		if i < 0 {
			break
		}
		line, after, _ := strings.Cut(stack[i:], "\n")
		if m := pythonFrameRe.FindStringSubmatch(line); m != nil {
			return m[1], m[2], after, true
		}
		end = i
	}
	return "", "", "", false
}

func firstExceptionLine(s string) (name, message string) {
	for line := range strings.Lines(s) {
		if name, message, ok := exceptionLine(strings.TrimRight(line, "\r\n")); ok {
			return name, message
		}
	}
	return "", ""
}

func exceptionLine(line string) (name, message string, ok bool) {
	m := exceptionLineRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[1], strings.TrimSpace(line[len(m[0]):]), true
}

func atFrameLabel(frame string) string {
	frame = packagingDataRe.ReplaceAllString(frame, "")
	fn, location := "", frame
	if open := trailingParenGroup(frame); open >= 0 {
		fn, location = strings.TrimSpace(frame[:open]), frame[open+1:len(frame)-1]
	}
	return frameLabel(basename(lineNumberRe.ReplaceAllString(location, "")), fn)
}

func trailingParenGroup(s string) int {
	if !strings.HasSuffix(s, ")") {
		return -1
	}
	depth := 0
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case ')':
			depth++
		case '(':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

func frameLabel(file, fn string) string {
	switch {
	case file == "":
		return fn
	case fn == "":
		return file
	}
	return file + " in " + fn
}

func basename(path string) string {
	return path[strings.LastIndexAny(path, `/\`)+1:]
}

func patternLine(errMessage, message, exceptionMessage string) string {
	line := firstLine(cmp.Or(errMessage, message))
	if strings.HasSuffix(line, pythonTracebackHeader) {
		return firstLine(exceptionMessage)
	}
	return line
}

// messagePattern redacts first so addresses, IPs and secrets collapse to one stable word instead of splitting groups per user.
func messagePattern(line string) string {
	return strings.Join(stableWords(withoutEnclosedSegments(redact.Text(line)), patternMaxWords), " ")
}

func withoutEnclosedSegments(s string) string {
	var b strings.Builder
	var quote byte
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote && closesQuote(s, i) {
				quote = 0
			}
			continue
		}
		switch {
		case opensQuote(s, i):
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth = max(depth-1, 0)
		case depth > 0:
			continue
		default:
			b.WriteByte(c)
			continue
		}
		b.WriteByte(' ')
	}
	return b.String()
}

func opensQuote(s string, i int) bool {
	switch s[i] {
	case '"', '`':
		return true
	case '\'':
		// A ' touching a word is an apostrophe (can't, users'), not a quote.
		return i == 0 || !isWordByte(s[i-1])
	}
	return false
}

func closesQuote(s string, i int) bool {
	return s[i] != '\'' || i+1 == len(s) || !isWordByte(s[i+1])
}

func stableWords(s string, limit int) []string {
	var words []string
	for i := 0; i < len(s) && len(words) < limit; {
		if !isWordByte(s[i]) && s[i] != '-' {
			i++
			continue
		}
		start := i
		for i < len(s) && (isWordByte(s[i]) || s[i] == '-') {
			i++
		}
		run := s[start:i]
		if strings.ContainsAny(run, "0123456789") {
			continue
		}
		for part := range strings.SplitSeq(run, "-") {
			if part != "" && len(words) < limit {
				words = append(words, part)
			}
		}
	}
	return words
}

func isWordByte(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func sampleMessage(message, errMessage string) string {
	message = strings.TrimSpace(message)
	switch {
	case errMessage == "" || strings.Contains(message, errMessage):
		return message
	case message == "":
		return errMessage
	}
	return message + "\n" + errMessage
}

func pythonTraceback(s string) string {
	if i := strings.Index(s, pythonTracebackHeader); i >= 0 {
		return strings.TrimSpace(s[i:])
	}
	return ""
}

func firstLine(s string) string {
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func attrString(attrs map[string]any, path string) string {
	switch v := attrValue(attrs, path).(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func attrValue(attrs map[string]any, path string) any {
	if v, ok := attrs[path]; ok && v != nil {
		return v
	}
	head, rest, nested := strings.Cut(path, ".")
	if !nested {
		return nil
	}
	child, _ := attrs[head].(map[string]any)
	return attrValue(child, rest)
}

func boundedOrDefault(n, def, limit int) int {
	if n <= 0 {
		return def
	}
	return min(n, limit)
}
