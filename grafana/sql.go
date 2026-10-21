package grafana

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/sqlguard"
)

const (
	defaultSQLRows = 100
	maxSQLRows     = 500
	sqlMaxPoints   = 100
)

// SYSTEM is not forbidden so system.* stays readable; a SYSTEM command can only lead a statement, which Lead refuses.
var readOnlyClickHouse = sqlguard.Rules{
	Lead: map[string]bool{
		"SELECT": true, "WITH": true, "SHOW": true, "DESCRIBE": true, "DESC": true, "EXISTS": true, "EXPLAIN": true,
	},
	Mutating: map[string]bool{
		"INSERT": true, "ALTER": true, "CREATE": true, "DROP": true, "DETACH": true, "ATTACH": true, "RENAME": true,
		"TRUNCATE": true, "OPTIMIZE": true, "GRANT": true, "REVOKE": true, "KILL": true, "UPDATE": true, "DELETE": true,
		"MOVE": true, "FREEZE": true, "UNFREEZE": true, "SET": true, "WATCH": true, "BACKUP": true, "RESTORE": true,
	},
	AllowedDesc:   "SELECT / WITH / SHOW / DESCRIBE / EXISTS / EXPLAIN",
	ForbiddenDesc: "INSERT / ALTER / CREATE / DROP / DELETE / SET",
}

var externalTableFuncRe = regexp.MustCompile(`(?i)\b(url|urlCluster|s3|s3Cluster|gcs|hdfs|hdfsCluster|azureBlobStorage|azureBlobStorageCluster|remote|remoteSecure|mysql|postgresql|jdbc|odbc|mongodb|redis|sqlite|file|fileCluster|executable|input|deltaLake|hudi|iceberg)\s*\(`)

// DatasourceRun is one datasource's outcome for a batch of named queries.
type DatasourceRun struct {
	Datasource Datasource
	Results    map[string]SQLResult
	Err        error
}

// SQLResult is one query's frames, or its error.
type SQLResult struct {
	SQL    string
	Frames []Frame
	Err    string
}

// Rows returns the total row count across frames.
func (r SQLResult) Rows() int {
	n := 0
	for _, f := range r.Frames {
		n += f.Rows
	}
	return n
}

type namedSQL struct {
	ref string
	sql string
}

func (c *Client) clickHouseTargets(ctx context.Context, refs []string) ([]Datasource, error) {
	all, err := c.ListDatasources(ctx, "clickhouse", "")
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, errors.New("no ClickHouse datasource is visible to the service account")
	}
	var out []Datasource
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		ds, err := matchDatasource(all, ref)
		if err != nil {
			return nil, err
		}
		if !seen[ds.UID] {
			seen[ds.UID] = true
			out = append(out, ds)
		}
	}
	if len(out) == 0 {
		out = all
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func matchDatasource(all []Datasource, ref string) (Datasource, error) {
	for _, d := range all {
		if d.UID == ref || strings.EqualFold(d.Name, ref) {
			return d, nil
		}
	}
	var hits []Datasource
	lower := strings.ToLower(ref)
	for _, d := range all {
		if strings.Contains(strings.ToLower(d.UID), lower) || strings.Contains(strings.ToLower(d.Name), lower) {
			hits = append(hits, d)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	names := make([]string, len(all))
	for i, d := range all {
		names[i] = d.UID
	}
	if len(hits) > 1 {
		return Datasource{}, fmt.Errorf("%q matches several ClickHouse datasources; use one of: %s", ref, strings.Join(names, ", "))
	}
	return Datasource{}, fmt.Errorf("no ClickHouse datasource matches %q; visible ones: %s", ref, strings.Join(names, ", "))
}

func (c *Client) runSQL(ctx context.Context, ds Datasource, from, to time.Time, interval time.Duration, queries []namedSQL) (map[string]SQLResult, error) {
	body := make([]map[string]any, len(queries))
	for i, q := range queries {
		body[i] = map[string]any{
			"refId":         q.ref,
			"datasource":    map[string]any{"uid": ds.UID, "type": ds.Type},
			"rawSql":        q.sql,
			"format":        1,
			"intervalMs":    interval.Milliseconds(),
			"maxDataPoints": sqlMaxPoints,
		}
	}
	resp, err := c.dsQuery(ctx, from, to, body)
	if err != nil {
		return nil, err
	}
	out := make(map[string]SQLResult, len(queries))
	for _, q := range queries {
		r := resp.Results[q.ref]
		res := SQLResult{SQL: q.sql, Err: c.queryError(ds, r.Error)}
		for _, f := range r.Frames {
			res.Frames = append(res.Frames, decodeFrame(f))
			if f.Schema.Meta.ExecutedQueryString != "" {
				res.SQL = f.Schema.Meta.ExecutedQueryString
			}
		}
		out[q.ref] = res
	}
	return out, nil
}

func (c *Client) fanOut(targets []Datasource, fn func(ds Datasource) (map[string]SQLResult, error)) []DatasourceRun {
	runs := make([]DatasourceRun, len(targets))
	runConcurrent(len(targets), maxConcurrentRuns, func(i int) {
		res, err := fn(targets[i])
		runs[i] = DatasourceRun{Datasource: targets[i], Results: res, Err: err}
	})
	return runs
}

var unreachableRe = regexp.MustCompile(`(?i)socks connect|host unreachable|connection refused|no route to host|i/o timeout|dial tcp|connection reset`)

const unreachableNotice = "Grafana cannot reach this datasource's database right now (connection failed). Other datasources are unaffected; retry later."

// Connection failures name internal hosts, so they are logged and replaced with a plain notice.
func (c *Client) queryError(ds Datasource, msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	if unreachableRe.MatchString(msg) {
		log.Printf("[grafana] datasource %s unreachable: %s", ds.UID, msg)
		return unreachableNotice
	}
	return strings.TrimPrefix(msg, "error querying the database: ")
}

// SQLQuery is one read-only SQL request.
type SQLQuery struct {
	Datasources []string
	SQL         string
	Start       string
	End         string
	Interval    string
	MaxRows     int
}

// SQLRunResult is the outcome of QuerySQL.
type SQLRunResult struct {
	From    time.Time
	To      time.Time
	MaxRows int
	Limited bool
	Runs    []DatasourceRun
}

// QuerySQL runs one read-only statement on each requested ClickHouse datasource.
func (c *Client) QuerySQL(ctx context.Context, q SQLQuery) (*SQLRunResult, error) {
	sql, err := checkReadOnly(q.SQL)
	if err != nil {
		return nil, err
	}
	targets, err := c.clickHouseTargets(ctx, q.Datasources)
	if err != nil {
		return nil, err
	}
	from, to, err := TimeRange(q.Start, q.End, time.Now())
	if err != nil {
		return nil, err
	}
	rows := clamp(q.MaxRows, 1, maxSQLRows, defaultSQLRows)
	interval := sqlInterval(from, to, q.Interval)
	sc := newScope(&Dashboard{}, nil, from, to, interval)
	sc.kind = "clickhouse"
	sql = sc.interpolate(sql)
	limited := false
	if lead := firstWord(sql); lead == "SELECT" || lead == "WITH" {
		sql, limited = withLimit(sql, rows+1)
	}
	res := &SQLRunResult{From: from, To: to, MaxRows: rows, Limited: limited}
	res.Runs = c.fanOut(targets, func(ds Datasource) (map[string]SQLResult, error) {
		return c.runSQL(ctx, ds, from, to, interval, []namedSQL{{ref: "A", sql: sql}})
	})
	return res, nil
}

func checkReadOnly(sql string) (string, error) {
	sql = strings.TrimSpace(sql)
	for strings.HasSuffix(sql, ";") {
		sql = strings.TrimSpace(strings.TrimSuffix(sql, ";"))
	}
	if sql == "" {
		return "", errors.New("sql is empty")
	}
	if err := readOnlyClickHouse.Validate(sql); err != nil {
		return "", err
	}
	if n := len(nonEmpty(sqlguard.Statements(sql))); n > 1 {
		return "", fmt.Errorf("send one statement per call, not %d", n)
	}
	if m := externalTableFuncRe.FindStringSubmatch(sql); m != nil {
		return "", fmt.Errorf("the %s() table function is not allowed: queries may only read tables in the database", m[1])
	}
	return sql, nil
}

func nonEmpty(stmts [][]string) [][]string {
	var out [][]string
	for _, s := range stmts {
		if len(s) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func firstWord(sql string) string {
	for _, s := range sqlguard.Statements(sql) {
		if len(s) > 0 {
			return s[0]
		}
	}
	return ""
}

// withLimit places LIMIT before a trailing SETTINGS or FORMAT clause, where ClickHouse requires it.
func withLimit(sql string, n int) (string, bool) {
	kw := topLevelKeywords(sql)
	if _, ok := kw["LIMIT"]; ok {
		return sql, false
	}
	at := len(sql)
	for _, k := range []string{"SETTINGS", "FORMAT"} {
		if pos, ok := kw[k]; ok && pos < at {
			at = pos
		}
	}
	clause := " LIMIT " + strconv.Itoa(n)
	if at == len(sql) {
		return sql + clause, true
	}
	return strings.TrimRight(sql[:at], " \t\r\n") + clause + " " + sql[at:], true
}

// topLevelKeywords maps words outside brackets, quotes and comments to their first offset; a word before "(" is a function.
func topLevelKeywords(sql string) map[string]int {
	out := map[string]int{}
	depth := 0
	for i := 0; i < len(sql); {
		ch := sql[i]
		switch {
		case ch == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case ch == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return out
			}
			i += end + 4
		case ch == '\'' || ch == '"' || ch == '`':
			i++
			for i < len(sql) && sql[i] != ch {
				if sql[i] == '\\' {
					i++
				}
				i++
			}
			i++
		case ch == '(' || ch == '[':
			depth++
			i++
		case ch == ')' || ch == ']':
			depth--
			i++
		case isWordStart(ch):
			j := i
			for j < len(sql) && isWordChar(sql[j]) {
				j++
			}
			if depth == 0 {
				word := strings.ToUpper(sql[i:j])
				k := j
				for k < len(sql) && (sql[k] == ' ' || sql[k] == '\t' || sql[k] == '\n' || sql[k] == '\r') {
					k++
				}
				if _, seen := out[word]; !seen && (k >= len(sql) || sql[k] != '(') {
					out[word] = i
				}
			}
			i = j
		default:
			i++
		}
	}
	return out
}

func isWordStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isWordChar(c byte) bool {
	return isWordStart(c) || (c >= '0' && c <= '9')
}

func sqlInterval(from, to time.Time, override string) time.Duration {
	if d, ok := parseInterval(override); ok {
		return d
	}
	d := roundInterval(to.Sub(from) / sqlMaxPoints)
	if d < time.Second {
		d = time.Second
	}
	return d
}

func chString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func chIdent(s string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(s) + "`"
}

func chTable(database, table string) string {
	return chIdent(database) + "." + chIdent(table)
}

func chList(vals []string) string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = chString(v)
	}
	return strings.Join(out, ", ")
}
