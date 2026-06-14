// Package databricks wraps the subset of the Databricks REST API that
// arbetern needs — currently the SQL Statement Execution API
// (/api/2.0/sql/statements). It lets an agent run a read-only SQL query
// against a Databricks SQL warehouse and get the rows back inline.
//
// Authentication uses OAuth 2.0 machine-to-machine (client credentials)
// against a workspace-level service principal: a client ID + client secret
// are exchanged at the workspace token endpoint ({host}/oidc/v1/token) for a
// short-lived bearer token (scope "all-apis"), sent via HTTP Basic auth.
// This mirrors the flow the official Databricks SDK uses. Tokens are cached
// in-process and refreshed shortly before they expire.
//
// A SQL warehouse ID selects the compute that runs the statement. The
// workspace URL, client ID, client secret and warehouse ID are all supplied
// via configuration (see config.Credentials).
package databricks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// oauthScope is the scope requested for workspace API access. "all-apis"
	// is what the Databricks SDK requests for service-principal M2M tokens.
	oauthScope = "all-apis"

	// defaultWaitTimeout is how long the initial execute call blocks waiting
	// for results before falling back to async polling. The API accepts 0s or
	// 5s–50s; 30s lets most interactive queries return in a single round-trip.
	defaultWaitTimeout = "30s"

	// pollInterval is the delay between GET polls once a statement has gone
	// async (state PENDING/RUNNING after the initial wait_timeout elapses).
	pollInterval = 2 * time.Second

	// maxPollDuration caps how long we keep polling a long-running statement
	// (e.g. an AI_FORECAST) before giving up and cancelling it.
	maxPollDuration = 4 * time.Minute

	// defaultRowLimit / maxRowLimit bound how many rows we return to the LLM.
	// INLINE disposition also caps the whole result set at 25 MiB server-side.
	defaultRowLimit = 1000
	maxRowLimit     = 10000

	// Response body size limits for io.LimitReader.
	maxResponseBody     = 64 << 20 // 64 MiB — result chunks can be large.
	maxAuthResponseBody = 1 << 20  // 1 MiB — token responses are tiny.

	// httpTimeout bounds a single HTTP round-trip. It must exceed
	// defaultWaitTimeout so the synchronous execute call isn't cut off.
	httpTimeout = 90 * time.Second
)

// Client talks to the Databricks SQL Statement Execution API for one
// workspace. It is safe for concurrent use.
type Client struct {
	host         string // normalized workspace base URL, e.g. "https://dbc-1234.cloud.databricks.com".
	clientID     string
	clientSecret string
	warehouseID  string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
	connected   bool // true once at least one OAuth token has been acquired.
}

// NewClient builds a Databricks client from a workspace URL and
// service-principal OAuth credentials. host may be supplied with or without a
// scheme/trailing slash. If the initial token fetch fails the client is still
// returned in a disconnected state and a background goroutine retries every
// 5 seconds until it succeeds, so the service never blocks on startup.
func NewClient(host, clientID, clientSecret, warehouseID string) *Client {
	c := &Client{
		host:         normalizeHost(host),
		clientID:     strings.TrimSpace(clientID),
		clientSecret: strings.TrimSpace(clientSecret),
		warehouseID:  strings.TrimSpace(warehouseID),
		httpClient:   &http.Client{Timeout: httpTimeout},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.token(ctx); err != nil {
		log.Printf("[databricks] initial OAuth failed, will retry every 5s: %v", err)
		go c.retryConnect()
	} else {
		log.Printf("[databricks] OAuth token acquired for %s (warehouse %s)", c.host, c.warehouseID)
	}
	return c
}

// Ready reports whether at least one OAuth token has been acquired. Mirrors
// the OAuthClient interface the command layer uses to gate tools.
func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Host returns the normalized workspace URL. Useful for integration-status panels.
func (c *Client) Host() string { return c.host }

// WarehouseID returns the configured SQL warehouse ID.
func (c *Client) WarehouseID() string { return c.warehouseID }

// retryConnect attempts to acquire an OAuth token every 5 seconds until it succeeds.
func (c *Client) retryConnect() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := c.token(ctx)
		cancel()
		if err != nil {
			log.Printf("[databricks] OAuth retry failed: %v", err)
			continue
		}
		log.Printf("[databricks] OAuth token acquired after retry for %s", c.host)
		return
	}
}

// --------------------------------------------------------------------------
// SQL statement execution
// --------------------------------------------------------------------------

// Query executes read-only SQL against the configured SQL warehouse and
// returns the rows. sqlText may be a script of several `;`-separated statements
// — e.g. DECLARE/SET session variables followed by a SELECT — in which case the
// warehouse returns the result of the final statement. params bind `:name`
// markers in the statement. rowLimit caps the rows returned (<=0 uses the
// default). The call blocks synchronously for up to defaultWaitTimeout and then
// polls until the statement reaches a terminal state or maxPollDuration
// elapses.
//
// Query rejects anything that is not clearly read-only (see
// validateReadOnlySQL): every statement must begin with a read-only keyword and
// none may contain a mutating verb. The tool is a reporting/query surface, and
// refusing mutations keeps an LLM-generated statement from accidentally writing
// to or dropping data even if the service principal has been granted more than
// it should.
func (c *Client) Query(ctx context.Context, sqlText string, params []QueryParam, rowLimit int) (*QueryResult, error) {
	sqlText = strings.TrimSpace(sqlText)
	if sqlText == "" {
		return nil, fmt.Errorf("statement is required")
	}
	if c.warehouseID == "" {
		return nil, fmt.Errorf("no SQL warehouse configured (set databricks-warehouse-id)")
	}
	if err := validateReadOnlySQL(sqlText); err != nil {
		return nil, err
	}
	if rowLimit <= 0 || rowLimit > maxRowLimit {
		rowLimit = defaultRowLimit
	}

	reqBody := map[string]any{
		"warehouse_id":    c.warehouseID,
		"statement":       sqlText,
		"wait_timeout":    defaultWaitTimeout,
		"on_wait_timeout": "CONTINUE",
		"disposition":     "INLINE",
		"format":          "JSON_ARRAY",
		"row_limit":       rowLimit,
	}
	if len(params) > 0 {
		ps := make([]map[string]any, 0, len(params))
		for _, p := range params {
			name := strings.TrimPrefix(strings.TrimSpace(p.Name), ":")
			if name == "" {
				continue
			}
			m := map[string]any{"name": name, "value": p.Value}
			if t := strings.TrimSpace(p.Type); t != "" {
				m["type"] = t
			}
			ps = append(ps, m)
		}
		if len(ps) > 0 {
			reqBody["parameters"] = ps
		}
	}

	var resp statementResponse
	// POST path must not have a trailing slash (a trailing slash returns 404).
	if err := c.doJSON(ctx, http.MethodPost, "/api/2.0/sql/statements", reqBody, &resp); err != nil {
		return nil, err
	}

	// Poll until the statement leaves PENDING/RUNNING.
	deadline := time.Now().Add(maxPollDuration)
	for isPending(resp.Status.State) {
		if time.Now().After(deadline) {
			// Best-effort cancel so the warehouse isn't left running our query.
			cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = c.cancel(cctx, resp.StatementID)
			ccancel()
			return nil, fmt.Errorf("statement %s did not complete within %s", resp.StatementID, maxPollDuration)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
		var polled statementResponse
		if err := c.doJSON(ctx, http.MethodGet, "/api/2.0/sql/statements/"+url.PathEscape(resp.StatementID), nil, &polled); err != nil {
			return nil, err
		}
		resp = polled
	}

	switch resp.Status.State {
	case "SUCCEEDED":
		// fall through to result assembly
	case "FAILED", "CANCELED", "CLOSED":
		msg := resp.Status.State
		if resp.Status.Error != nil && strings.TrimSpace(resp.Status.Error.Message) != "" {
			msg = resp.Status.Error.Message
		}
		return nil, fmt.Errorf("statement %s: %s", resp.Status.State, msg)
	default:
		return nil, fmt.Errorf("unexpected statement state %q", resp.Status.State)
	}

	out := &QueryResult{WarehouseID: c.warehouseID, StatementID: resp.StatementID}
	if resp.Manifest != nil {
		out.Truncated = resp.Manifest.Truncated
		for _, col := range resp.Manifest.Schema.Columns {
			t := col.TypeText
			if t == "" {
				t = col.TypeName
			}
			out.Columns = append(out.Columns, Column{Name: col.Name, Type: t})
		}
	}

	// Walk result chunks, capping at rowLimit.
	chunk := resp.Result
collect:
	for chunk != nil {
		for _, row := range chunk.DataArray {
			if len(out.Rows) >= rowLimit {
				out.Truncated = true
				break collect
			}
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
		if chunk.NextChunkIndex == nil {
			break
		}
		var next resultData
		path := fmt.Sprintf("/api/2.0/sql/statements/%s/result/chunks/%d", url.PathEscape(resp.StatementID), *chunk.NextChunkIndex)
		if err := c.doJSON(ctx, http.MethodGet, path, nil, &next); err != nil {
			return nil, err
		}
		chunk = &next
	}

	out.RowCount = len(out.Rows)
	return out, nil
}

// cancel asks Databricks to stop executing a statement (best effort).
func (c *Client) cancel(ctx context.Context, statementID string) error {
	if statementID == "" {
		return nil
	}
	return c.doJSON(ctx, http.MethodPost, "/api/2.0/sql/statements/"+url.PathEscape(statementID)+"/cancel", nil, nil)
}

// isPending reports whether a statement state is still in progress.
func isPending(state string) bool {
	return state == "PENDING" || state == "RUNNING"
}

// --------------------------------------------------------------------------
// HTTP + auth helpers
// --------------------------------------------------------------------------

// doJSON performs an authenticated JSON request against the workspace. A nil
// body sends no payload; a nil out discards the response.
func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.host+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("databricks request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("read databricks response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("databricks HTTP %d for %s %s: %s", resp.StatusCode, method, path, truncate(string(data), 500))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse databricks response: %w", err)
	}
	return nil
}

// token returns a cached OAuth access token, refreshing it when it is missing
// or within 60s of expiry. Uses the workspace token endpoint with the
// client-credentials grant and HTTP Basic auth (client ID / secret).
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Until(c.tokenExpiry) > 60*time.Second {
		return c.accessToken, nil
	}
	if c.host == "" {
		return "", fmt.Errorf("databricks host is not configured")
	}
	if c.clientID == "" || c.clientSecret == "" {
		return "", fmt.Errorf("databricks client ID and client secret are required")
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("scope", oauthScope)
	endpoint := c.host + "/oidc/v1/token"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.clientID, c.clientSecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("databricks token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAuthResponseBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("databricks token endpoint returned %s: %s", resp.Status, truncate(string(body), 300))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parse databricks token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("databricks token endpoint returned no access_token")
	}

	c.accessToken = tr.AccessToken
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	c.tokenExpiry = time.Now().Add(ttl)
	c.connected = true
	return c.accessToken, nil
}

// normalizeHost ensures the workspace URL has an https scheme and no trailing
// slash. Accepts bare hosts ("dbc-1234.cloud.databricks.com") and full URLs.
func normalizeHost(raw string) string {
	h := strings.TrimSpace(raw)
	h = strings.TrimRight(h, "/")
	if h == "" {
		return ""
	}
	if !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") {
		h = "https://" + h
	}
	return h
}

// readOnlyLeadKeywords are the statement-leading verbs we permit. Anything not
// listed here (INSERT, UPDATE, DELETE, MERGE, CREATE, DROP, ALTER, TRUNCATE,
// GRANT, …) is rejected by default. DECLARE / SET / USE / BEGIN / END allow the
// session-setup statements that commonly precede a reporting SELECT in a
// multi-statement script — e.g. `DECLARE OR REPLACE VARIABLE … ; WITH … SELECT`.
var readOnlyLeadKeywords = map[string]bool{
	"SELECT": true, "WITH": true, "SHOW": true, "DESCRIBE": true, "DESC": true,
	"EXPLAIN": true, "VALUES": true, "TABLE": true, "DECLARE": true, "SET": true,
	"USE": true, "BEGIN": true, "END": true,
}

// mutatingKeywords are verbs that write data, change schema, or alter
// privileges. They are rejected even when they appear AFTER an allowed leading
// keyword — e.g. inside a `WITH … INSERT` CTE or a `BEGIN … END` block — so a
// read-only-looking prefix can't smuggle a mutation past the leading-keyword
// check. Read-only modifiers such as REPLACE (`DECLARE OR REPLACE VARIABLE`)
// and transaction words like COMMIT are intentionally excluded so they don't
// reject legitimate reporting queries.
var mutatingKeywords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "UPSERT": true,
	"CREATE": true, "DROP": true, "ALTER": true, "TRUNCATE": true, "UNDROP": true,
	"RENAME": true, "GRANT": true, "REVOKE": true, "COPY": true, "LOAD": true,
	"OVERWRITE": true, "RESTORE": true, "OPTIMIZE": true, "VACUUM": true,
	"REPAIR": true, "MSCK": true, "REFRESH": true, "CACHE": true, "UNCACHE": true,
}

// validateReadOnlySQL checks that every statement in sqlText is read-only.
// sqlText may be a script of several `;`-separated statements: each one must
// begin with a keyword in readOnlyLeadKeywords AND must not contain any
// mutatingKeyword anywhere outside string literals, quoted identifiers and
// comments. It returns nil when the whole script is read-only, or an error
// naming the first offending keyword. A script with no statement (only
// comments / whitespace) is rejected.
func validateReadOnlySQL(sqlText string) error {
	sawStatement := false
	for _, toks := range scanSQLStatements(sqlText) {
		if len(toks) == 0 {
			continue
		}
		sawStatement = true
		if !readOnlyLeadKeywords[toks[0]] {
			return fmt.Errorf("statement starting with %q is not allowed; only read-only statements may run (SELECT / WITH / SHOW / DESCRIBE / EXPLAIN / VALUES, plus DECLARE / SET / USE for session setup)", toks[0])
		}
		for _, t := range toks {
			if mutatingKeywords[t] {
				return fmt.Errorf("statement contains the mutating keyword %q; INSERT / UPDATE / DELETE / MERGE / CREATE / DROP / ALTER and other write operations are forbidden", t)
			}
		}
	}
	if !sawStatement {
		return fmt.Errorf("no SQL statement found")
	}
	return nil
}

// scanSQLStatements splits sql into statements at top-level semicolons and
// returns, for each statement, the uppercased word tokens that appear OUTSIDE
// of string literals ('…' and "…"), quoted identifiers (`…`) and comments
// (-- … and /* … */). Tokens are all the read-only check needs, and tracking
// quote/comment state in a single pass means a `;`, keyword or `:marker`
// hiding inside a string or comment can never affect statement boundaries or
// keyword detection.
func scanSQLStatements(sql string) [][]string {
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
			// contents so nothing inside is tokenized. Handle doubled-quote
			// escapes for all three, and backslash escapes for string
			// literals (Spark's default), but not for `…` identifiers.
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
