// Package google wraps the narrow slice of Google Workspace that arbetern
// surfaces to agents: Google Sheets (read a range, append rows) and the Drive
// file index needed to resolve a spreadsheet by name.
//
// Three properties shape the whole package:
//
//   - Service-account auth, no interactive OAuth. arbetern runs headless in a
//     pod, so the only usable flow is the JWT-bearer grant (RFC 7523): sign a
//     short-lived assertion with the service account's RSA key and exchange it
//     for an access token. There is no domain-wide delegation — the account
//     acts as itself, never impersonating a user.
//
//   - Folder-scoped by construction. The service account is given access to one
//     Drive folder (or shared drive) and nothing else, and every call in this
//     package is additionally checked against that folder ID in code before it
//     is sent. Drive's own ACL is the real boundary; the in-code check is
//     defense in depth, because a Sheets call addresses a spreadsheet by ID and
//     would otherwise never consult the folder tree at all.
//
//   - Batched and rate-limit aware. The Sheets API allows 60 write and 300 read
//     requests per minute per user. One scheduled workflow tick can target many
//     spreadsheets, so writes are grouped so that N rows cost one request per
//     (spreadsheet, tab), a client-side token budget keeps the process under
//     quota, and 429/5xx responses are retried with exponential backoff instead
//     of failing the tick.
package google

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/safego"
)

// Google API endpoints. Variables rather than constants so a test can point
// them at a stub server; never reassigned in production code.
var (
	sheetsBase = "https://sheets.googleapis.com/v4"
	driveBase  = "https://www.googleapis.com/drive/v3"
)

const (
	// DefaultScopes is the narrowest scope pair that supports the access
	// pattern the tools need: write cell values in a spreadsheet, and look a
	// spreadsheet up by name in the shared folder.
	//
	// drive.file is narrower still, but it authorises only files the app itself
	// created or that were individually granted through a picker — it cannot
	// enumerate the contents of a folder that was shared with the account, so
	// drive_find_file would return nothing. drive.readonly is therefore the
	// Drive scope, and it is not as broad as it reads: the service account has
	// no Drive of its own and no visibility beyond what is explicitly shared
	// with it, so "read every file you can see" means "read the shared folder".
	DefaultScopes = "https://www.googleapis.com/auth/spreadsheets https://www.googleapis.com/auth/drive.readonly"

	// defaultTokenURI is used when the credentials JSON omits token_uri.
	defaultTokenURI = "https://oauth2.googleapis.com/token"

	// assertionTTL is the lifetime of a signed JWT assertion. Google caps it at
	// one hour; the returned access token carries its own (also ~1h) expiry.
	assertionTTL = time.Hour

	// tokenSkew refreshes the access token this long before it actually expires
	// so a long tick cannot start a call with a token that dies mid-flight.
	tokenSkew = 5 * time.Minute

	// maxResponseBody caps every response read (io.LimitReader).
	maxResponseBody = 10 << 20 // 10 MB

	httpTimeout = 45 * time.Second

	// userAgent identifies arbetern in Google's API logs.
	userAgent = "arbetern/google-connector"

	// Per-minute request budgets enforced client-side, set just under the
	// documented Sheets quotas (60 writes, 300 reads per minute per user) so a
	// burst of workflow ticks throttles itself instead of collecting 429s.
	writeBudgetPerMin = 55
	readBudgetPerMin  = 250

	// maxRetries bounds the retry chain for one request (the initial attempt
	// plus this many retries).
	maxRetries = 5

	// parentsCacheTTL is how long a file's resolved folder ancestry is trusted.
	// A file's parents change rarely, and re-checking on every call would double
	// the request count of every write.
	parentsCacheTTL = 10 * time.Minute

	// rootsCacheTTL is how long the discovered set of shared folders / drives
	// and their subtrees is trusted. Shares change on human timescales, so a
	// newly shared folder becomes visible within this window without a restart.
	rootsCacheTTL = 5 * time.Minute
)

// serviceAccount is the subset of a Google service-account key file we use.
type serviceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	ClientID     string `json:"client_id"`
	TokenURI     string `json:"token_uri"`
}

// Client talks to Google Sheets and Drive as a single service account.
//
// Scope comes from what has been SHARED with the service account: the client
// discovers those folders and shared drives at first use and adopts them as its
// working set (see drive.go). pinnedFolders, when configured, overrides that
// discovery and confines the connector to exactly those folders.
type Client struct {
	sa            *serviceAccount
	privateKey    *rsa.PrivateKey
	scopes        string
	pinnedFolders []string // empty = discover whatever is shared with the account
	httpClient    *http.Client

	// Token state.
	tokenMu     sync.RWMutex
	accessToken string
	tokenExpiry time.Time
	connected   bool

	writes *budget
	reads  *budget

	// parentsCache memoises "may the connector touch this file", keyed by file
	// ID. See withinScope.
	parentsMu    sync.Mutex
	parentsCache map[string]parentsEntry

	// roots caches the top-level shared folders / drives.
	rootsMu      sync.Mutex
	roots        []Root
	rootsFetched time.Time

	// folderTree caches every in-scope folder ID (each root plus its
	// descendants), so a file in a subfolder is still recognised as in-scope.
	treeMu      sync.Mutex
	folderTree  map[string]bool
	treeFetched time.Time
}

type parentsEntry struct {
	inScope   bool
	reason    string // why it was refused, for a cached negative verdict
	fetchedAt time.Time
}

// NewClient builds a Google client from a service-account key.
//
// credentialsJSON is the key file, either raw JSON or base64-encoded (the shape
// `base64 < key.json` produces) — both are accepted because the base64 form is
// what fits in a Kubernetes Secret value without newline mangling.
//
// folderIDs is OPTIONAL and comma- or whitespace-separated. Left empty (the
// normal case) the client discovers every Drive folder and shared drive that has
// been shared with the service account and works across all of them — share a
// folder with the account's email address and it becomes available with no
// config change. Supplying folder IDs pins the connector to exactly those
// folders and makes the in-code guard refuse everything else, even other folders
// the account can see.
//
// scopes defaults to DefaultScopes when empty.
//
// A failed initial token exchange is not fatal: the client is returned
// disconnected and a background goroutine retries every 5 seconds, matching how
// every other arbetern connector behaves during a bad-credentials rollout.
func NewClient(credentialsJSON, folderIDs, scopes string) (*Client, error) {
	sa, key, err := parseServiceAccount(credentialsJSON)
	if err != nil {
		return nil, err
	}
	if scopes = normalizeScopes(scopes); scopes == "" {
		scopes = DefaultScopes
	}

	c := &Client{
		sa:            sa,
		privateKey:    key,
		scopes:        scopes,
		pinnedFolders: splitList(folderIDs),
		httpClient:    &http.Client{Timeout: httpTimeout},
		writes:        newBudget(writeBudgetPerMin),
		reads:         newBudget(readBudgetPerMin),
		parentsCache:  make(map[string]parentsEntry),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.refreshToken(ctx); err != nil {
		log.Printf("[google] initial token exchange failed, will retry every 5s: %v", err)
		safego.Go("google: connect retry", c.retryConnect)
		return c, nil
	}
	scope := "all folders shared with the account"
	if len(c.pinnedFolders) > 0 {
		scope = fmt.Sprintf("pinned to %d folder(s)", len(c.pinnedFolders))
	}
	log.Printf("[google] authenticated as %s (project %s, scope: %s)", sa.ClientEmail, sa.ProjectID, scope)
	return c, nil
}

// splitList splits a comma- or whitespace-separated list, dropping blanks.
func splitList(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// parseServiceAccount decodes the credentials blob (base64 or raw JSON) and
// parses the PEM private key out of it.
func parseServiceAccount(credentialsJSON string) (*serviceAccount, *rsa.PrivateKey, error) {
	raw := strings.TrimSpace(credentialsJSON)
	if raw == "" {
		return nil, nil, fmt.Errorf("google credentials JSON is empty")
	}
	// Base64 is the expected form; raw JSON is accepted for local dev. Sniffing
	// the first byte is enough to tell them apart — a key file always starts
	// with '{', and base64 of '{' never does.
	if !strings.HasPrefix(raw, "{") {
		// Tolerate the line wrapping some base64 tools emit.
		compact := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, raw)
		decoded, err := base64.StdEncoding.DecodeString(compact)
		if err != nil {
			if decoded, err = base64.RawStdEncoding.DecodeString(compact); err != nil {
				return nil, nil, fmt.Errorf("google credentials are neither JSON nor valid base64: %w", err)
			}
		}
		raw = strings.TrimSpace(string(decoded))
	}

	var sa serviceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return nil, nil, fmt.Errorf("parsing google credentials JSON: %w", err)
	}
	if sa.Type != "" && sa.Type != "service_account" {
		return nil, nil, fmt.Errorf("google credentials are of type %q; a service_account key is required (arbetern is headless and cannot run an interactive OAuth flow)", sa.Type)
	}
	if sa.ClientEmail == "" {
		return nil, nil, fmt.Errorf("google credentials are missing client_email")
	}
	if sa.PrivateKey == "" {
		return nil, nil, fmt.Errorf("google credentials are missing private_key")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = defaultTokenURI
	}

	key, err := parseRSAPrivateKey(sa.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	return &sa, key, nil
}

// parseRSAPrivateKey decodes the PEM-encoded PKCS#8 (or PKCS#1) RSA key that
// Google puts in the `private_key` field.
func parseRSAPrivateKey(pemKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, fmt.Errorf("google private_key is not valid PEM (check that the \\n escapes survived the secret round-trip)")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("google private_key is a %T; an RSA key is required", key)
		}
		return rsaKey, nil
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing google private_key: %w", err)
	}
	return key, nil
}

// normalizeScopes accepts a comma- or whitespace-separated scope list and
// returns the space-separated form the token endpoint expects.
func normalizeScopes(scopes string) string {
	fields := strings.FieldsFunc(scopes, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	return strings.Join(fields, " ")
}

// ── Identity / status ──────────────────────────────────────────────────────

// Ready reports whether at least one access token has been obtained.
func (c *Client) Ready() bool {
	if c == nil {
		return false
	}
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.connected
}

// ServiceAccountEmail returns the identity the connector acts as. This is the
// address the Drive folder must be shared with.
func (c *Client) ServiceAccountEmail() string {
	if c == nil || c.sa == nil {
		return ""
	}
	return c.sa.ClientEmail
}

// ProjectID returns the GCP project the service account belongs to.
func (c *Client) ProjectID() string {
	if c == nil || c.sa == nil {
		return ""
	}
	return c.sa.ProjectID
}

// PinnedFolders returns the folder IDs this deployment is confined to, or nil
// when scope is discovered from what has been shared with the service account.
func (c *Client) PinnedFolders() []string {
	if c == nil {
		return nil
	}
	return c.pinnedFolders
}

// Scopes returns the OAuth scopes the access token is minted with.
func (c *Client) Scopes() string {
	if c == nil {
		return ""
	}
	return c.scopes
}

// ── Token exchange (JWT-bearer grant) ──────────────────────────────────────

// retryConnect retries the token exchange every 5 seconds until it succeeds.
func (c *Client) retryConnect() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := c.refreshToken(ctx)
		cancel()
		if err != nil {
			log.Printf("[google] token retry failed: %v", err)
			continue
		}
		log.Printf("[google] authenticated after retry as %s", c.sa.ClientEmail)
		return
	}
}

// refreshToken signs a fresh JWT assertion and exchanges it for an access token.
func (c *Client) refreshToken(ctx context.Context) error {
	assertion, err := c.signAssertion(time.Now())
	if err != nil {
		return err
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("reading token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint returned HTTP %d: %s", resp.StatusCode, snippet(body))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return fmt.Errorf("decoding token response: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("token response carried no access_token: %s", snippet(body))
	}
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = assertionTTL
	}

	c.tokenMu.Lock()
	c.accessToken = tok.AccessToken
	c.tokenExpiry = time.Now().Add(ttl)
	c.connected = true
	c.tokenMu.Unlock()
	return nil
}

// signAssertion builds and RS256-signs the JWT the token endpoint trades for an
// access token.
func (c *Client) signAssertion(now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": c.sa.PrivateKeyID}
	claims := map[string]any{
		"iss":   c.sa.ClientEmail,
		"scope": c.scopes,
		"aud":   c.sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(assertionTTL).Unix(),
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("encoding JWT header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encoding JWT claims: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing JWT assertion: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// token returns a valid access token, refreshing it when it is close to expiry.
func (c *Client) token(ctx context.Context) (string, error) {
	c.tokenMu.RLock()
	tok, expiry, connected := c.accessToken, c.tokenExpiry, c.connected
	c.tokenMu.RUnlock()

	if connected && time.Now().Add(tokenSkew).Before(expiry) {
		return tok, nil
	}
	if err := c.refreshToken(ctx); err != nil {
		if connected {
			// A refresh failure with a still-valid token in hand is not fatal.
			return tok, nil
		}
		return "", err
	}
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.accessToken, nil
}

// ── Request plumbing (budget + retry/backoff) ──────────────────────────────

// budget is a fixed-window request allowance. Callers wait for a slot before
// issuing a request, which keeps arbetern under Google's per-minute quota
// rather than discovering it through 429s.
type budget struct {
	mu    sync.Mutex
	limit int
	times []time.Time // issue times inside the trailing window, oldest first
}

func newBudget(limit int) *budget {
	return &budget{limit: limit, times: make([]time.Time, 0, limit)}
}

// wait blocks until a request may be issued, or ctx is done.
func (b *budget) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := time.Now()
		cutoff := now.Add(-time.Minute)
		keep := b.times[:0]
		for _, t := range b.times {
			if t.After(cutoff) {
				keep = append(keep, t)
			}
		}
		b.times = keep
		if len(b.times) < b.limit {
			b.times = append(b.times, now)
			b.mu.Unlock()
			return nil
		}
		sleep := time.Until(b.times[0].Add(time.Minute)) + 50*time.Millisecond
		b.mu.Unlock()

		if sleep <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}
}

// apiRequest issues an authenticated Google API request and decodes the JSON
// body into out. write selects which per-minute budget the call draws from.
// Retryable failures (429 and 5xx) are retried with exponential backoff and
// jitter, honouring Retry-After when the response supplies it.
func (c *Client) apiRequest(ctx context.Context, method, fullURL string, payload any, out any, write bool) error {
	if c == nil {
		return fmt.Errorf("google client is not configured")
	}

	var bodyBytes []byte
	if payload != nil {
		var err error
		if bodyBytes, err = json.Marshal(payload); err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
	}

	bkt := c.reads
	if write {
		bkt = c.writes
	}

	// nextWait carries the backoff for the upcoming attempt so a server-supplied
	// Retry-After can override the exponential schedule, and so the delay is
	// applied exactly once per retry.
	var (
		lastErr  error
		nextWait time.Duration
	)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := sleepCtx(ctx, nextWait); err != nil {
			return err
		}
		nextWait = backoff(attempt+1, 0)

		if err := bkt.wait(ctx); err != nil {
			return err
		}

		token, err := c.token(ctx)
		if err != nil {
			return fmt.Errorf("acquiring google access token: %w", err)
		}

		var reader io.Reader
		if bodyBytes != nil {
			reader = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
		if err != nil {
			return fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", userAgent)
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("google request failed: %w", err)
			continue // transport error — worth a retry
		}

		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("reading google response: %w", readErr)
			continue
		}

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out == nil || len(respBody) == 0 {
				return nil
			}
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("decoding google response: %w", err)
			}
			return nil

		case retryableStatus(resp.StatusCode) && attempt < maxRetries:
			lastErr = apiError(resp.StatusCode, respBody)
			nextWait = backoff(attempt+1, retryAfter(resp.Header.Get("Retry-After")))
			log.Printf("[google] retrying in %s (attempt %d/%d) on %s — %s",
				nextWait.Round(time.Millisecond), attempt+1, maxRetries, redactURL(fullURL), ErrorDetail(lastErr))
			continue

		default:
			return apiError(resp.StatusCode, respBody)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("google request exhausted %d attempts", maxRetries+1)
	}
	return lastErr
}

// retryableStatus reports whether a status code is worth retrying: 429 (rate
// limit / quota) and the transient 5xx family. Everything else — 400 bad range,
// 403 not shared with the service account, 404 wrong ID — is a caller error
// that retrying only delays.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff returns the delay before the given attempt: exponential (1s, 2s, 4s,
// 8s, 16s) capped at 30s, with up to 250ms of jitter so parallel targets in one
// tick do not retry in lockstep. A server-supplied Retry-After wins when longer.
func backoff(attempt int, serverHint time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Second << (attempt - 1)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	if serverHint > d {
		d = serverHint
	}
	if d > time.Minute {
		d = time.Minute
	}
	return d + jitter(250*time.Millisecond)
}

// jitter returns a uniform random duration in [0, max), using crypto/rand
// because the package pulls it in for signing anyway.
func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return max / 2
	}
	return time.Duration(n.Int64())
}

// retryAfter parses a Retry-After header in its delay-seconds or HTTP-date form.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// googleAPIError is the error envelope both Sheets and Drive return.
type googleAPIError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// APIError is a non-2xx response from Sheets or Drive.
//
// It deliberately separates two audiences. Error() is what reaches the model and
// therefore a Slack channel: a short, actionable sentence with no internals in
// it. Detail() is what belongs in the pod log: Google's own message, which names
// the real fault and is what an operator needs. Google's raw text is useful for
// debugging and unhelpful-to-alarming in a channel — "Invalid field selection
// sharedWithMe" reads to a user as a broken product, and to a model as something
// worth editorialising about, when it is a connector bug they can do nothing
// with.
type APIError struct {
	Status       int    // HTTP status
	GoogleStatus string // e.g. "INVALID_ARGUMENT", "PERMISSION_DENIED"
	Message      string // Google's own message — log surface only
	actionable   bool   // whether Message describes something the CALLER got wrong
}

// Detail returns the full diagnostic form, for logs.
func (e *APIError) Detail() string {
	if e.Message == "" {
		return fmt.Sprintf("google API error: HTTP %d", e.Status)
	}
	return fmt.Sprintf("google API error (HTTP %d %s): %s", e.Status, e.GoogleStatus, e.Message)
}

// Error returns the sanitized, model-facing form.
func (e *APIError) Error() string {
	switch {
	case e.actionable:
		// The caller's own arguments are wrong and Google says exactly how, so
		// passing that through is what lets the model fix itself.
		return fmt.Sprintf("the request was rejected: %s", e.Message)
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		return "access was denied. The usual cause is that the target was never shared with this connector's Google service account, or was shared read-only while the call needs to write. This is a sharing/configuration matter — retrying will not help."
	case e.Status == http.StatusNotFound:
		return "the file or spreadsheet was not found. Either the ID is wrong, or it has not been shared with this connector. Re-resolve it with drive_find_file and use only IDs that returns."
	case e.Status == http.StatusTooManyRequests:
		return "Google rate-limited the request and it did not succeed after several retries. Wait a minute and retry with a smaller batch."
	case e.Status >= 500:
		return "Google returned a temporary server error that did not clear after several retries. Retry shortly."
	default:
		// A malformed request the caller could not have caused: a bad field
		// selector, an invalid query. Say it is a defect and stop the model from
		// reporting it as missing data or retrying it forever.
		return fmt.Sprintf("the Google connector issued a request Google rejected (HTTP %d). This is a defect in arbetern, not a problem with your request or with the data — do not retry it, and report that the Google integration needs a fix.", e.Status)
	}
}

// callerFaultRE matches the Google messages that describe a mistake in the
// CALLER's arguments — a bad A1 range, a missing tab — as opposed to a request
// this package built wrongly. Only these are echoed to the model verbatim.
var callerFaultRE = regexp.MustCompile(`(?i)unable to parse range|invalid range|no grid with id|requested entity was not found|invalid value at|exceeds grid limits|must be less than or equal to`)

// connectorFaultRE matches messages that indicate this package sent a malformed
// request. These are never echoed: they name our own field selectors and query
// syntax, which tells a user nothing and a model less.
var connectorFaultRE = regexp.MustCompile(`(?i)invalid field selection|invalid query|unknown field|invalid parameter|cannot parse the query`)

// apiError converts a non-2xx response into an *APIError.
func apiError(status int, body []byte) error {
	e := &APIError{Status: status}
	var parsed googleAPIError
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		e.GoogleStatus = parsed.Error.Status
		e.Message = parsed.Error.Message
	} else {
		e.Message = snippet(body)
	}
	if status == http.StatusBadRequest &&
		callerFaultRE.MatchString(e.Message) && !connectorFaultRE.MatchString(e.Message) {
		e.actionable = true
	}
	return e
}

// ErrorDetail returns the log-surface text for err: an *APIError's full Google
// message, or the error's own text for anything else. Call sites log this and
// surface err.Error() to the model.
func ErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Detail()
	}
	return err.Error()
}

// snippet trims a response body for inclusion in an error message.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// redactURL strips the query string from a URL before it reaches a log line, so
// a spreadsheet range or search term never lands in the pod logs.
func redactURL(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// logf is a thin wrapper so non-fatal Drive discovery problems are reported
// consistently without every call site importing log.
func logf(format string, a ...any) {
	log.Printf(format, a...)
}
