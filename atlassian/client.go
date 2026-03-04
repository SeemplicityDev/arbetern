package atlassian

import (
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

// authMode controls how API requests are authenticated.
type authMode string

const (
	authBasic authMode = "basic"
	authOAuth authMode = "oauth"

	// Atlassian OAuth 2.0 (3LO) token endpoint.
	atlassianTokenURL        = "https://auth.atlassian.com/oauth/token"
	atlassianResourcesURL    = "https://api.atlassian.com/oauth/token/accessible-resources"
	atlassianOAuthAPIBaseURL = "https://api.atlassian.com/ex/jira"

	// Confluence OAuth 2.0 API base URL (same cloud ID as Jira).
	atlassianConfluenceOAuthBase = "https://api.atlassian.com/ex/confluence"

	// Response body size limits for io.LimitReader.
	maxResponseBody     = 10 << 20 // 10 MB — general API responses
	maxAuthResponseBody = 5 << 20  // 5 MB  — OAuth / token responses

	// Pagination page sizes.
	jiraPageSize             = 100 // Jira caps maxResults at 100 per page
	confluenceSpacesPageSize = 50
)

// Client provides access to the Atlassian Cloud REST APIs (Jira + Confluence).
type Client struct {
	baseURL    string // API base URL for REST calls (differs between Basic Auth and OAuth)
	siteURL    string // human-readable site URL (e.g. "https://yourorg.atlassian.net") — used for browse links
	email      string // used for Basic Auth
	apiToken   string // used for Basic Auth
	projectKey string // default project key
	httpClient *http.Client
	mode       authMode

	// OAuth 2.0 fields (only used when mode == authOAuth).
	clientID     string
	clientSecret string
	cloudID      string // Atlassian cloud ID resolved from accessible-resources
	accessToken  string
	tokenExpiry  time.Time
	connected    bool // true once OAuth handshake + cloud ID resolution succeeds
	tokenMu      sync.RWMutex

	// Cached custom field IDs (discovered lazily).
	extraFieldsOnce sync.Once
	teamFieldID     string // e.g. "customfield_10001"
	sprintFieldID   string // e.g. "customfield_10020"
}

// NewClient creates an Atlassian API client using Basic Auth (email + API token).
// Basic Auth clients are always considered connected (no OAuth handshake required).
func NewClient(baseURL, email, apiToken, defaultProject string) *Client {
	cleanURL := strings.TrimRight(baseURL, "/")
	return &Client{
		baseURL:    cleanURL,
		siteURL:    cleanURL,
		email:      email,
		apiToken:   apiToken,
		projectKey: defaultProject,
		httpClient: &http.Client{},
		mode:       authBasic,
		connected:  true,
	}
}

// NewOAuthClient creates an Atlassian API client using OAuth 2.0 client credentials.
// If the initial OAuth handshake or cloud-ID resolution fails the client is
// returned in a disconnected state and a background goroutine retries every
// 5 seconds until it succeeds.  The service is never blocked.
func NewOAuthClient(baseURL, clientID, clientSecret, defaultProject string) *Client {
	cleanURL := strings.TrimRight(baseURL, "/")
	c := &Client{
		siteURL:      cleanURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		projectKey:   defaultProject,
		httpClient:   &http.Client{},
		mode:         authOAuth,
	}

	if err := c.connectOAuth(); err != nil {
		log.Printf("[atlassian] initial OAuth failed, will retry every 5s: %v", err)
		go c.retryConnect()
	}

	return c
}

// connectOAuth performs the full OAuth handshake: token fetch + cloud-ID resolution.
func (c *Client) connectOAuth() error {
	if err := c.refreshToken(); err != nil {
		return fmt.Errorf("token fetch: %w", err)
	}
	log.Printf("[atlassian] OAuth token acquired (expires %s)", c.tokenExpiry.Format(time.RFC3339))

	cloudID, err := c.resolveCloudID()
	if err != nil {
		return fmt.Errorf("cloud ID resolution for %s: %w", c.siteURL, err)
	}

	c.tokenMu.Lock()
	c.cloudID = cloudID
	c.baseURL = fmt.Sprintf("%s/%s", atlassianOAuthAPIBaseURL, cloudID)
	c.connected = true
	c.tokenMu.Unlock()

	log.Printf("[atlassian] OAuth cloud ID resolved: %s -> %s", c.siteURL, c.baseURL)
	return nil
}

// retryConnect attempts the full OAuth handshake every 5 seconds until it succeeds.
func (c *Client) retryConnect() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := c.connectOAuth(); err != nil {
			log.Printf("[atlassian] OAuth retry failed: %v", err)
			continue
		}
		log.Printf("[atlassian] OAuth connected after retry")
		return
	}
}

// Ready reports whether the client has successfully completed its OAuth
// handshake (or is using Basic Auth, in which case it is always ready).
func (c *Client) Ready() bool {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.connected
}

// resolveCloudID calls the Atlassian accessible-resources endpoint to find the
// cloud ID matching the configured site URL.
func (c *Client) resolveCloudID() (string, error) {
	req, err := http.NewRequest(http.MethodGet, atlassianResourcesURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthResponseBody))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("accessible-resources returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var resources []struct {
		ID   string `json:"id"`
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &resources); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	if len(resources) == 0 {
		return "", fmt.Errorf("no accessible Atlassian sites found -- ensure the OAuth app is authorized for your site")
	}

	// Match by URL.
	siteNorm := strings.TrimRight(strings.ToLower(c.siteURL), "/")
	for _, r := range resources {
		if strings.TrimRight(strings.ToLower(r.URL), "/") == siteNorm {
			log.Printf("[atlassian] matched site %q -> cloud ID %s (name: %s)", c.siteURL, r.ID, r.Name)
			return r.ID, nil
		}
	}

	// If only one site, use it.
	if len(resources) == 1 {
		log.Printf("[atlassian] WARN: site URL %q didn't match %q, using the only available site (cloud ID: %s)", c.siteURL, resources[0].URL, resources[0].ID)
		return resources[0].ID, nil
	}

	names := make([]string, len(resources))
	for i, r := range resources {
		names[i] = fmt.Sprintf("%s (%s)", r.URL, r.ID)
	}
	return "", fmt.Errorf("site URL %q not found in accessible resources: %v", c.siteURL, names)
}

// AuthMode returns the authentication mode ("basic" or "oauth").
func (c *Client) AuthMode() string {
	return string(c.mode)
}

// refreshToken fetches a new OAuth access token using the client credentials grant.
func (c *Client) refreshToken() error {
	payload := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}

	resp, err := c.httpClient.PostForm(atlassianTokenURL, payload)
	if err != nil {
		return fmt.Errorf("token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthResponseBody))
	if err != nil {
		return fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"` // seconds
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("unmarshal token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return fmt.Errorf("empty access token in response: %s", string(body))
	}

	c.tokenMu.Lock()
	c.accessToken = tokenResp.AccessToken
	// Refresh 60 seconds before actual expiry to avoid edge-case failures.
	c.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn)*time.Second - 60*time.Second)
	c.tokenMu.Unlock()

	return nil
}

// getAccessToken returns a valid OAuth access token, refreshing if needed.
func (c *Client) getAccessToken() (string, error) {
	c.tokenMu.RLock()
	if !c.connected {
		c.tokenMu.RUnlock()
		return "", fmt.Errorf("atlassian client not connected (OAuth pending)")
	}
	token := c.accessToken
	expiry := c.tokenExpiry
	c.tokenMu.RUnlock()

	if time.Now().Before(expiry) {
		return token, nil
	}

	log.Printf("[atlassian] OAuth token expired, refreshing...")
	if err := c.refreshToken(); err != nil {
		return "", err
	}
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.accessToken, nil
}

// authRequest sets the appropriate authentication headers on a request.
func (c *Client) authRequest(req *http.Request) error {
	switch c.mode {
	case authOAuth:
		token, err := c.getAccessToken()
		if err != nil {
			return fmt.Errorf("get OAuth token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	default: // authBasic
		req.SetBasicAuth(c.email, c.apiToken)
	}
	return nil
}

// DefaultProject returns the configured default project key.
func (c *Client) DefaultProject() string {
	return c.projectKey
}
