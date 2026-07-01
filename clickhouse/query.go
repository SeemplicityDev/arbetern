package clickhouse

// Read-only SQL over the ClickHouse HTTP interface
// (https://clickhouse.com/docs/en/interfaces/http). Separate from the Cloud
// billing API in client.go: it POSTs SELECT / SHOW / DESCRIBE / EXISTS to a
// service's HTTPS endpoint. The tool is deliberately generic — any mapping of
// a name to a database or table lives in the agent's prompt, not here.
//
// Read-only is enforced two ways: a SELECT-only database user (recommended in
// the docs) and validateReadOnlyCH, which rejects any non-read-only statement
// before it is sent.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// defaultQueryRowLimit / maxQueryRowLimit bound the rows a query returns.
	// ClickHouse enforces the cap server-side via max_result_rows.
	defaultQueryRowLimit = 1000
	maxQueryRowLimit     = 10000

	// queryFormat returns metadata plus rows with every value as a string.
	queryFormat = "JSONCompactStrings"
)

// SQLColumn describes one column of a query result set.
type SQLColumn struct {
	Name string `json:"name"`
	Type string `json:"type"` // ClickHouse type, e.g. "UInt64", "String", "Nullable(DateTime)".
}

// SQLQueryResult is the flattened, LLM-friendly result of a read-only query.
type SQLQueryResult struct {
	Columns   []SQLColumn `json:"columns"`
	Rows      [][]string  `json:"rows"` // each cell is the string form of the value; SQL NULL is rendered as "NULL".
	RowCount  int         `json:"row_count"`
	Truncated bool        `json:"truncated"`
	Endpoint  string      `json:"endpoint"`
}

// chQueryResponse is the JSONCompactStrings envelope ClickHouse returns.
type chQueryResponse struct {
	Meta []SQLColumn `json:"meta"`
	Data [][]*string `json:"data"` // *string so a JSON null (SQL NULL) is distinguishable from "".
	Rows int         `json:"rows"`
}

// Query runs read-only SQL against the configured service endpoint and returns
// the rows. rowLimit caps the rows (<=0 uses the default; above the max is
// clamped). It rejects anything not clearly read-only (see validateReadOnlyCH).
func (c *Client) Query(ctx context.Context, sqlText string, rowLimit int) (*SQLQueryResult, error) {
	sqlText = strings.TrimSpace(sqlText)
	if sqlText == "" {
		return nil, fmt.Errorf("statement is required")
	}
	if c.queryEndpoint == "" || c.queryUser == "" {
		return nil, fmt.Errorf("ClickHouse SQL query interface is not configured (set clickhouse-query-endpoint and clickhouse-query-user)")
	}
	if err := validateReadOnlyCH(sqlText); err != nil {
		return nil, err
	}
	if rowLimit <= 0 || rowLimit > maxQueryRowLimit {
		rowLimit = defaultQueryRowLimit
	}

	var resp chQueryResponse
	if err := c.runQuery(ctx, sqlText, rowLimit, &resp); err != nil {
		return nil, err
	}

	out := &SQLQueryResult{
		Endpoint: c.queryEndpoint,
		Columns:  resp.Meta,
	}
	for _, row := range resp.Data {
		cells := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				cells[i] = "NULL"
			} else {
				cells[i] = *v
			}
		}
		out.Rows = append(out.Rows, cells)
	}
	out.RowCount = len(out.Rows)
	// A full page (rows == cap) means the result was truncated server-side.
	out.Truncated = out.RowCount >= rowLimit
	return out, nil
}

// pingQuery validates the SQL interface with a trivial SELECT 1, then marks
// the client query-connected.
func (c *Client) pingQuery(ctx context.Context) error {
	if c.queryEndpoint == "" || c.queryUser == "" {
		return fmt.Errorf("missing query endpoint or user")
	}
	var discard chQueryResponse
	return c.runQuery(ctx, "SELECT 1", 1, &discard)
}

// runQuery POSTs sqlText to the ClickHouse HTTP interface, sets the arbetern
// User-Agent, caps the result server-side, and decodes the response into out.
func (c *Client) runQuery(ctx context.Context, sqlText string, rowLimit int, out *chQueryResponse) error {
	q := url.Values{}
	q.Set("default_format", queryFormat)
	q.Set("max_result_rows", fmt.Sprintf("%d", rowLimit))
	q.Set("result_overflow_mode", "break")
	u := c.queryEndpoint + "/?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(sqlText))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.SetBasicAuth(c.queryUser, c.queryPassword)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("clickhouse query request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return queryError(resp.StatusCode, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	c.markQueryConnected()
	return nil
}

// queryError converts a non-2xx response into an error. The query interface
// returns a plain-text "Code: N. DB::Exception: …" body.
func queryError(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	if msg == "" {
		return fmt.Errorf("clickhouse query error: HTTP %d", status)
	}
	return fmt.Errorf("clickhouse query error (HTTP %d): %s", status, msg)
}

// readOnlyLeadKeywordsCH are the statement-leading verbs the query tool
// permits. Anything else (INSERT, ALTER, CREATE, DROP, …) is rejected.
var readOnlyLeadKeywordsCH = map[string]bool{
	"SELECT": true, "WITH": true, "SHOW": true, "DESCRIBE": true, "DESC": true,
	"EXISTS": true, "EXPLAIN": true, "VALUES": true,
}

// mutatingKeywordsCH are verbs that write data or change schema/privileges/
// session state. They are rejected even after an allowed leading keyword (e.g.
// a `WITH … INSERT`) so a read-only-looking prefix can't smuggle a mutation.
var mutatingKeywordsCH = map[string]bool{
	"INSERT": true, "ALTER": true, "CREATE": true, "DROP": true, "DETACH": true,
	"ATTACH": true, "RENAME": true, "TRUNCATE": true, "OPTIMIZE": true, "GRANT": true,
	"REVOKE": true, "KILL": true, "SYSTEM": true, "UPDATE": true, "DELETE": true,
	"MOVE": true, "FREEZE": true, "UNFREEZE": true, "SET": true, "WATCH": true,
}

// validateReadOnlyCH checks that every `;`-separated statement begins with an
// allowed lead keyword and contains no mutating keyword (outside strings,
// quoted identifiers and comments). It returns an error naming the first
// offender.
func validateReadOnlyCH(sqlText string) error {
	sawStatement := false
	for _, toks := range scanCHStatements(sqlText) {
		if len(toks) == 0 {
			continue
		}
		sawStatement = true
		if !readOnlyLeadKeywordsCH[toks[0]] {
			return fmt.Errorf("statement starting with %q is not allowed; only read-only statements may run (SELECT / WITH / SHOW / DESCRIBE / EXISTS / EXPLAIN / VALUES)", toks[0])
		}
		for _, t := range toks {
			if mutatingKeywordsCH[t] {
				return fmt.Errorf("statement contains the mutating keyword %q; INSERT / ALTER / CREATE / DROP / DELETE / SET and other write operations are forbidden", t)
			}
		}
	}
	if !sawStatement {
		return fmt.Errorf("no SQL statement found")
	}
	return nil
}

// scanCHStatements splits sql at top-level semicolons and returns the
// uppercased word tokens of each statement, ignoring anything inside string
// literals, quoted identifiers (`…`) and comments so they can't affect
// statement boundaries or keyword detection.
func scanCHStatements(sql string) [][]string {
	var (
		stmts [][]string
		cur   []string
		tok   strings.Builder
	)
	flushTok := func() {
		if tok.Len() > 0 {
			cur = append(cur, strings.ToUpper(tok.String()))
			tok.Reset()
		}
	}
	flushStmt := func() {
		flushTok()
		stmts = append(stmts, cur)
		cur = nil
	}

	i, n := 0, len(sql)
	for i < n {
		ch := sql[i]
		switch {
		case ch == '-' && i+1 < n && sql[i+1] == '-':
			// Line comment: skip to end of line.
			flushTok()
			i += 2
			for i < n && sql[i] != '\n' {
				i++
			}
		case ch == '/' && i+1 < n && sql[i+1] == '*':
			// Block comment: skip to closing */.
			flushTok()
			i += 2
			for i+1 < n && (sql[i] != '*' || sql[i+1] != '/') {
				i++
			}
			i += 2
		case ch == '\'' || ch == '"' || ch == '`':
			// Quoted span: string literal or quoted identifier. Skip its
			// contents. Handle doubled-quote escapes for all three and
			// backslash escapes inside string literals.
			flushTok()
			quote := ch
			backslashEscapes := quote == '\'' || quote == '"'
			i++
			for i < n {
				if backslashEscapes && sql[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if sql[i] == quote {
					if i+1 < n && sql[i+1] == quote {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case ch == ';':
			flushStmt()
			i++
		case (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_':
			tok.WriteByte(ch)
			i++
		case ch >= '0' && ch <= '9' && tok.Len() > 0:
			// A digit extends an identifier already started (e.g. t1); a digit
			// that starts a token is part of a number, which we skip.
			tok.WriteByte(ch)
			i++
		default:
			flushTok()
			i++
		}
	}
	flushStmt()
	return stmts
}
