// Package databricks wraps the Databricks SQL Statement Execution API
// (/api/2.0/sql/statements) so an agent can run read-only SQL against a SQL
// warehouse and get the rows inline.
//
// Auth is OAuth 2.0 machine-to-machine: a service-principal client ID/secret
// are exchanged at {host}/oidc/v1/token (scope "all-apis") for a short-lived
// bearer token, cached in-process and refreshed before expiry. A warehouse ID
// selects the compute. All are supplied via config.Credentials.
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
	"sort"
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

	// userAgent identifies arbetern to Databricks (sent on every request) so
	// queries are attributable in the workspace's audit logs.
	userAgent = "arbetern/databricks-connector"
)

// Client talks to the Databricks SQL Statement Execution API. It holds a
// default workspace + warehouse; a single query may target another allow-listed
// workspace via QueryOptions. It is safe for concurrent use.
type Client struct {
	host         string // normalized default workspace base URL, e.g. "https://dbc-1234.cloud.databricks.com".
	clientID     string
	clientSecret string
	warehouseID  string   // default SQL warehouse, used when a query names none.
	allowedHosts []string // normalized workspace hosts a per-query override may target.
	httpClient   *http.Client

	mu         sync.Mutex
	tokens     map[string]*hostToken // OAuth token cache, keyed by workspace host.
	warehouses map[string]string     // discovered warehouse ID, keyed by workspace host.
	connected  bool                  // true once the DEFAULT host has yielded a token.
}

// hostToken is one workspace's cached bearer token. Tokens are per-workspace —
// each is minted at that workspace's own {host}/oidc/v1/token and rejected by
// any other.
type hostToken struct {
	accessToken string
	expiry      time.Time
}

// NewClient builds a Databricks client from a workspace URL and
// service-principal OAuth credentials. host may be supplied with or without a
// scheme/trailing slash. allowedHosts are the extra workspaces a per-query
// override may target; empty pins every query to host. If the initial token
// fetch fails the client is still returned in a disconnected state and a
// background goroutine retries every 5 seconds, so startup never blocks.
func NewClient(host, clientID, clientSecret, warehouseID string, allowedHosts []string) *Client {
	c := &Client{
		host:         normalizeHost(host),
		clientID:     strings.TrimSpace(clientID),
		clientSecret: strings.TrimSpace(clientSecret),
		warehouseID:  strings.TrimSpace(warehouseID),
		allowedHosts: normalizeHosts(allowedHosts),
		httpClient:   &http.Client{Timeout: httpTimeout},
		tokens:       make(map[string]*hostToken),
		warehouses:   make(map[string]string),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.token(ctx, c.host); err != nil {
		log.Printf("[databricks] initial OAuth failed, will retry every 5s: %v", err)
		go c.retryConnect()
	} else {
		log.Printf("[databricks] OAuth token acquired for %s (warehouse %s)", c.host, c.warehouseID)
	}
	if len(c.allowedHosts) > 0 {
		log.Printf("[databricks] per-query workspace overrides allowed for: %s", strings.Join(c.allowedHosts, ", "))
	}
	return c
}

// Ready reports whether the DEFAULT workspace has yielded an OAuth token, so
// an unreachable override workspace never un-advertises the tool. Mirrors the
// OAuthClient interface the command layer uses to gate tools.
func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Host returns the normalized default workspace URL.
func (c *Client) Host() string { return c.host }

// WarehouseID returns the default SQL warehouse ID.
func (c *Client) WarehouseID() string { return c.warehouseID }

// AllowedHosts returns the override targets, excluding the default host.
func (c *Client) AllowedHosts() []string {
	out := make([]string, len(c.allowedHosts))
	copy(out, c.allowedHosts)
	return out
}

// retryConnect attempts to acquire an OAuth token every 5 seconds until it succeeds.
func (c *Client) retryConnect() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := c.token(ctx, c.host)
		cancel()
		if err != nil {
			log.Printf("[databricks] OAuth retry failed: %v", err)
			continue
		}
		log.Printf("[databricks] OAuth token acquired after retry for %s", c.host)
		return
	}
}

// resolveTarget picks the workspace and warehouse a query runs against. Only
// the host is gated; the warehouse is resolved in three steps, because a
// warehouse ID names compute inside ONE workspace and is meaningless in any
// other: an explicit override wins, the configured default applies only to the
// default workspace, and anything else is discovered from the target workspace.
func (c *Client) resolveTarget(ctx context.Context, hostOverride, warehouseOverride string) (string, string, error) {
	host := c.host
	if h := normalizeHost(hostOverride); h != "" {
		if err := c.allowHost(h); err != nil {
			return "", "", err
		}
		host = c.canonicalHost(h)
	}
	if host == "" {
		return "", "", fmt.Errorf("databricks host is not configured")
	}

	if w := strings.TrimSpace(warehouseOverride); w != "" {
		return host, w, nil
	}
	if host == c.host && c.warehouseID != "" {
		return host, c.warehouseID, nil
	}
	warehouse, err := c.discoverWarehouse(ctx, host)
	if err != nil {
		return "", "", err
	}
	return host, warehouse, nil
}

// discoverWarehouse picks a SQL warehouse in host that this principal can use,
// so a query against a secondary workspace does not need its warehouse ID
// hardcoded. Prefers a running warehouse, then falls back to the first by name
// for a stable choice across ticks. Cached per workspace for the process
// lifetime — the set of warehouses changes far more slowly than queries run.
func (c *Client) discoverWarehouse(ctx context.Context, host string) (string, error) {
	c.mu.Lock()
	cached := c.warehouses[host]
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	var resp warehousesResponse
	if err := c.doJSON(ctx, host, http.MethodGet, "/api/2.0/sql/warehouses", nil, &resp); err != nil {
		return "", fmt.Errorf("no warehouse_id given and listing warehouses on %s failed: %w", hostLabel(host), err)
	}
	usable := make([]warehouseInfo, 0, len(resp.Warehouses))
	for _, w := range resp.Warehouses {
		if w.ID != "" && w.State != "DELETED" && w.State != "DELETING" {
			usable = append(usable, w)
		}
	}
	if len(usable) == 0 {
		return "", fmt.Errorf("no warehouse_id given and this service principal can see no usable SQL warehouse on %s — grant it CAN USE on one, or pass warehouse_id", hostLabel(host))
	}

	sort.SliceStable(usable, func(i, j int) bool {
		ri, rj := usable[i].State == "RUNNING", usable[j].State == "RUNNING"
		if ri != rj {
			return ri
		}
		return usable[i].Name < usable[j].Name
	})
	pick := usable[0]

	c.mu.Lock()
	if c.warehouses == nil {
		c.warehouses = make(map[string]string)
	}
	c.warehouses[host] = pick.ID
	c.mu.Unlock()
	log.Printf("[databricks] using warehouse %q (%s, state=%s) on %s", pick.Name, pick.ID, pick.State, hostLabel(host))
	return pick.ID, nil
}

// canonicalHost maps a permitted host onto its configured spelling so
// token-cache keys don't fragment on case.
func (c *Client) canonicalHost(host string) string {
	if strings.EqualFold(host, c.host) {
		return c.host
	}
	for _, h := range c.allowedHosts {
		if strings.EqualFold(h, host) {
			return h
		}
	}
	return host
}

// allowHost reports whether an override may target host. The default is always
// permitted; anything else must be allow-listed. This is a security boundary,
// not validation: the client secret is Basic-auth'd to {host}/oidc/v1/token, so
// an unchecked host would be a credential-exfiltration path.
func (c *Client) allowHost(host string) error {
	// Hostnames are case-insensitive; a differently-cased URL is still valid.
	if strings.EqualFold(host, c.host) {
		return nil
	}
	if len(c.allowedHosts) == 0 {
		return fmt.Errorf("workspace override to %q is not permitted: no additional hosts are allow-listed (set databricks-allowed-hosts / DATABRICKS_ALLOWED_HOSTS to the workspaces this service principal may query)", host)
	}
	permitted := false
	for _, h := range c.allowedHosts {
		if strings.EqualFold(h, host) {
			permitted = true
			break
		}
	}
	if !permitted {
		return fmt.Errorf("workspace override to %q is not permitted; allow-listed workspaces are: %s", host, strings.Join(c.allowedHosts, ", "))
	}
	return validateWorkspaceHost(host)
}

// --------------------------------------------------------------------------
// SQL statement execution
// --------------------------------------------------------------------------

// Query executes read-only SQL against a SQL warehouse and returns the rows.
// sqlText may be a `;`-separated script (e.g. DECLARE/SET then SELECT); the
// warehouse returns the final statement's result. params bind `:name` markers.
// opts selects the workspace/warehouse (empty = defaults) and caps the rows.
// The call blocks up to defaultWaitTimeout, then polls until terminal or
// maxPollDuration. It rejects anything not clearly read-only.
func (c *Client) Query(ctx context.Context, sqlText string, params []QueryParam, opts QueryOptions) (*QueryResult, error) {
	sqlText = strings.TrimSpace(sqlText)
	if sqlText == "" {
		return nil, fmt.Errorf("statement is required")
	}
	host, warehouseID, err := c.resolveTarget(ctx, opts.Host, opts.WarehouseID)
	if err != nil {
		return nil, err
	}
	if err := validateReadOnlySQL(sqlText); err != nil {
		return nil, err
	}
	rowLimit := opts.RowLimit
	if rowLimit <= 0 || rowLimit > maxRowLimit {
		rowLimit = defaultRowLimit
	}

	reqBody := map[string]any{
		"warehouse_id":    warehouseID,
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
	if err := c.doJSON(ctx, host, http.MethodPost, "/api/2.0/sql/statements", reqBody, &resp); err != nil {
		return nil, err
	}

	// Poll until the statement leaves PENDING/RUNNING.
	deadline := time.Now().Add(maxPollDuration)
	for isPending(resp.Status.State) {
		if time.Now().After(deadline) {
			// Best-effort cancel so the warehouse isn't left running our query.
			cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = c.cancel(cctx, host, resp.StatementID)
			ccancel()
			return nil, fmt.Errorf("statement %s did not complete within %s", resp.StatementID, maxPollDuration)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
		var polled statementResponse
		if err := c.doJSON(ctx, host, http.MethodGet, "/api/2.0/sql/statements/"+url.PathEscape(resp.StatementID), nil, &polled); err != nil {
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

	out := &QueryResult{Host: host, WarehouseID: warehouseID, StatementID: resp.StatementID}
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
		if err := c.doJSON(ctx, host, http.MethodGet, path, nil, &next); err != nil {
			return nil, err
		}
		chunk = &next
	}

	out.RowCount = len(out.Rows)
	return out, nil
}

// cancel asks Databricks to stop executing a statement (best effort).
func (c *Client) cancel(ctx context.Context, host, statementID string) error {
	if statementID == "" {
		return nil
	}
	return c.doJSON(ctx, host, http.MethodPost, "/api/2.0/sql/statements/"+url.PathEscape(statementID)+"/cancel", nil, nil)
}

// isPending reports whether a statement state is still in progress.
func isPending(state string) bool {
	return state == "PENDING" || state == "RUNNING"
}

// --------------------------------------------------------------------------
// HTTP + auth helpers
// --------------------------------------------------------------------------

// doJSON performs an authenticated JSON request against one workspace. A nil
// body sends no payload; a nil out discards the response. host must already
// have cleared resolveTarget, and every call of one Query must reuse it —
// statement IDs are workspace-scoped.
func (c *Client) doJSON(ctx context.Context, host, method, path string, body any, out any) error {
	tok, err := c.token(ctx, host)
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

	req, err := http.NewRequestWithContext(ctx, method, host+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
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

// token returns a cached OAuth access token for one workspace, refreshing it
// when missing or within 60s of expiry. Uses that workspace's token endpoint
// with the client-credentials grant and Basic auth, so the same service
// principal must be a member of every workspace targeted.
func (c *Client) token(ctx context.Context, host string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if host == "" {
		return "", fmt.Errorf("databricks host is not configured")
	}
	if c.tokens == nil {
		c.tokens = make(map[string]*hostToken)
	}
	if t := c.tokens[host]; t != nil && t.accessToken != "" && time.Until(t.expiry) > 60*time.Second {
		return t.accessToken, nil
	}
	if c.clientID == "" || c.clientSecret == "" {
		return "", fmt.Errorf("databricks client ID and client secret are required")
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("scope", oauthScope)
	endpoint := host + "/oidc/v1/token"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
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

	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	c.tokens[host] = &hostToken{accessToken: tr.AccessToken, expiry: time.Now().Add(ttl)}
	// Readiness tracks the DEFAULT workspace only: an override workspace that
	// happens to authenticate must not mark an unreachable default as healthy.
	if host == c.host {
		c.connected = true
	}
	return tr.AccessToken, nil
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

// normalizeHosts drops blanks, duplicates and unusable entries. A rejected
// entry is logged, not fatal, so one bad allow-list value can't break boot.
func normalizeHosts(raw []string) []string {
	var out []string
	seen := make(map[string]bool, len(raw))
	for _, r := range raw {
		h := normalizeHost(r)
		if h == "" || seen[h] {
			continue
		}
		if err := validateWorkspaceHost(h); err != nil {
			log.Printf("[databricks] ignoring allow-listed host %q: %v", r, err)
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// databricksDomains are the parent domains Databricks serves workspaces on.
var databricksDomains = []string{
	".cloud.databricks.com",    // AWS
	".gcp.databricks.com",      // GCP
	".azuredatabricks.net",     // Azure
	".databricks.com",          // vanity / account-level deployment names
	".databricks.azure.us",     // Azure Government
	".databricks.illinois.gov", // sovereign deployments
}

// validateWorkspaceHost checks that host is a bare https origin on a
// Databricks-owned domain — no path, query or userinfo to redirect the token
// exchange elsewhere.
func validateWorkspaceHost(host string) error {
	u, err := url.Parse(host)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("must use https")
	}
	// The account-console URL (https://<vanity>/?o=<workspace-id>) routes the
	// browser to a workspace but is not a REST/OAuth host; it is the obvious
	// thing to copy, so name the fix instead of just rejecting it.
	if u.Query().Has("o") {
		return fmt.Errorf("looks like an account-console URL; use the workspace's own URL instead (SQL warehouse → Connection details → Server hostname), not https://<host>/?o=<workspace-id>")
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be a bare workspace origin (https://<workspace-host>) with no path, query or credentials")
	}
	name := strings.ToLower(u.Hostname())
	if name == "" {
		return fmt.Errorf("missing hostname")
	}
	for _, d := range databricksDomains {
		if strings.HasSuffix(name, d) {
			return nil
		}
	}
	return fmt.Errorf("not a Databricks workspace domain")
}

// readOnlyLeadKeywords are the statement-leading verbs we permit; anything
// else is rejected. DECLARE / SET / USE / BEGIN / END cover the session setup
// that can precede a reporting SELECT in a multi-statement script.
var readOnlyLeadKeywords = map[string]bool{
	"SELECT": true, "WITH": true, "SHOW": true, "DESCRIBE": true, "DESC": true,
	"EXPLAIN": true, "VALUES": true, "TABLE": true, "DECLARE": true, "SET": true,
	"USE": true, "BEGIN": true, "END": true,
}

// mutatingKeywords are verbs that write data, change schema or alter
// privileges. They are rejected even after an allowed leading keyword (e.g. a
// `WITH … INSERT` CTE). REPLACE (`DECLARE OR REPLACE VARIABLE`) and COMMIT are
// intentionally excluded so they don't reject legitimate reporting queries.
var mutatingKeywords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "UPSERT": true,
	"CREATE": true, "DROP": true, "ALTER": true, "TRUNCATE": true, "UNDROP": true,
	"RENAME": true, "GRANT": true, "REVOKE": true, "COPY": true, "LOAD": true,
	"OVERWRITE": true, "RESTORE": true, "OPTIMIZE": true, "VACUUM": true,
	"REPAIR": true, "MSCK": true, "REFRESH": true, "CACHE": true, "UNCACHE": true,
}

// validateReadOnlySQL checks that every `;`-separated statement begins with an
// allowed lead keyword and contains no mutating keyword (outside strings,
// quoted identifiers and comments). It returns an error naming the first
// offender; a script with no statement is rejected.
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

// scanSQLStatements splits sql at top-level semicolons and returns the
// uppercased word tokens of each statement, ignoring anything inside string
// literals, quoted identifiers (`…`) and comments so they can't affect
// statement boundaries or keyword detection.
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
