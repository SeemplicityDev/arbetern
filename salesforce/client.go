package salesforce

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

const (
	defaultAPIVersion = "v62.0"
	defaultLoginURL   = "https://login.salesforce.com"
)

// Client provides access to the Salesforce REST API using OAuth 2.0 client credentials.
type Client struct {
	loginURL    string // e.g. "https://login.salesforce.com" or "https://test.salesforce.com"
	instanceURL string // resolved after token fetch, e.g. "https://yourorg.my.salesforce.com"
	apiVersion  string // e.g. "v62.0"
	httpClient  *http.Client

	// OAuth 2.0 client credentials.
	consumerKey    string
	consumerSecret string

	// Token management.
	accessToken string
	tokenExpiry time.Time
	tokenMu     sync.RWMutex
}

// NewClient creates a Salesforce API client using the OAuth 2.0 client credentials flow.
// It fetches an initial access token and resolves the instance URL.
func NewClient(consumerKey, consumerSecret, loginURL string) (*Client, error) {
	if loginURL == "" {
		loginURL = defaultLoginURL
	}
	c := &Client{
		loginURL:       strings.TrimRight(loginURL, "/"),
		apiVersion:     defaultAPIVersion,
		consumerKey:    consumerKey,
		consumerSecret: consumerSecret,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}

	if err := c.refreshToken(); err != nil {
		return nil, fmt.Errorf("initial Salesforce token fetch failed: %w", err)
	}
	log.Printf("[salesforce] OAuth token acquired, instance URL: %s (expires %s)",
		c.instanceURL, c.tokenExpiry.Format(time.RFC3339))

	return c, nil
}

// refreshToken fetches a new OAuth access token using the client credentials grant.
func (c *Client) refreshToken() error {
	tokenURL := c.loginURL + "/services/oauth2/token"

	payload := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.consumerKey},
		"client_secret": {c.consumerSecret},
	}

	resp, err := c.httpClient.PostForm(tokenURL, payload)
	if err != nil {
		return fmt.Errorf("token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		InstanceURL string `json:"instance_url"`
		IssuedAt    string `json:"issued_at"` // Unix timestamp in milliseconds
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("unmarshal token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return fmt.Errorf("empty access token in response: %s", string(body))
	}
	if tokenResp.InstanceURL == "" {
		return fmt.Errorf("empty instance_url in response: %s", string(body))
	}

	c.tokenMu.Lock()
	c.accessToken = tokenResp.AccessToken
	c.instanceURL = strings.TrimRight(tokenResp.InstanceURL, "/")
	// Salesforce client_credentials tokens typically last 2 hours.
	// Refresh 5 minutes before expiry to avoid edge-case failures.
	c.tokenExpiry = time.Now().Add(115 * time.Minute)
	c.tokenMu.Unlock()

	return nil
}

// getAccessToken returns a valid OAuth access token, refreshing if needed.
func (c *Client) getAccessToken() (string, error) {
	c.tokenMu.RLock()
	token := c.accessToken
	expiry := c.tokenExpiry
	c.tokenMu.RUnlock()

	if time.Now().Before(expiry) {
		return token, nil
	}

	log.Printf("[salesforce] OAuth token expired, refreshing...")
	if err := c.refreshToken(); err != nil {
		return "", err
	}
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.accessToken, nil
}

// apiRequest makes an authenticated request to the Salesforce REST API.
func (c *Client) apiRequest(method, path string, body io.Reader) (*http.Response, error) {
	token, err := c.getAccessToken()
	if err != nil {
		return nil, fmt.Errorf("get OAuth token: %w", err)
	}

	fullURL := c.instanceURL + path
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.httpClient.Do(req)
}

// ── SOQL Query API ─────────────────────────────────────────────────────────

// QueryResult is the response envelope from the Salesforce Query API.
type QueryResult struct {
	TotalSize      int              `json:"totalSize"`
	Done           bool             `json:"done"`
	NextRecordsURL string           `json:"nextRecordsUrl"`
	Records        []map[string]any `json:"records"`
}

// Query executes a SOQL query and returns all results (follows nextRecordsUrl automatically).
func (c *Client) Query(soql string) (*QueryResult, error) {
	path := fmt.Sprintf("/services/data/%s/query?q=%s", c.apiVersion, url.QueryEscape(soql))

	var allRecords []map[string]any
	totalSize := 0

	for {
		resp, err := c.apiRequest(http.MethodGet, path, nil)
		if err != nil {
			return nil, fmt.Errorf("query request failed: %w", err)
		}

		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if err != nil {
			return nil, fmt.Errorf("read query response: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			// Parse Salesforce error format.
			var sfErrors []SFError
			if json.Unmarshal(respBody, &sfErrors) == nil && len(sfErrors) > 0 {
				return nil, fmt.Errorf("SOQL query failed: %s — %s", sfErrors[0].ErrorCode, sfErrors[0].Message)
			}
			return nil, fmt.Errorf("query returned HTTP %d: %s", resp.StatusCode, string(respBody))
		}

		var result QueryResult
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("unmarshal query response: %w", err)
		}

		allRecords = append(allRecords, result.Records...)
		totalSize = result.TotalSize

		if result.Done || result.NextRecordsURL == "" {
			break
		}
		path = result.NextRecordsURL
	}

	return &QueryResult{
		TotalSize: totalSize,
		Done:      true,
		Records:   allRecords,
	}, nil
}

// SFError represents a Salesforce REST API error.
type SFError struct {
	Message   string   `json:"message"`
	ErrorCode string   `json:"errorCode"`
	Fields    []string `json:"fields"`
}

// ── Describe API ───────────────────────────────────────────────────────────

// FieldInfo describes a single field on a Salesforce object.
type FieldInfo struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Type  string `json:"type"`
}

// DescribeResult holds the basic describe response for an SObject.
type DescribeResult struct {
	Name   string      `json:"name"`
	Label  string      `json:"label"`
	Fields []FieldInfo `json:"fields"`
}

// Describe returns metadata (fields, labels, types) for a Salesforce object.
func (c *Client) Describe(objectName string) (*DescribeResult, error) {
	path := fmt.Sprintf("/services/data/%s/sobjects/%s/describe", c.apiVersion, url.PathEscape(objectName))

	resp, err := c.apiRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("describe request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read describe response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("describe returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result DescribeResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal describe response: %w", err)
	}

	return &result, nil
}

// ── Identity / Validation ──────────────────────────────────────────────────

// UserInfo holds the identity of the authenticated Salesforce user.
type UserInfo struct {
	UserID      string `json:"user_id"`
	OrgID       string `json:"organization_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	UserType    string `json:"user_type"`
	IsActive    bool   `json:"active"`
	OrgName     string `json:"-"`
	InstanceURL string `json:"-"`
}

// GetIdentity calls the Salesforce identity endpoint to verify connectivity
// and return information about the authenticated user.
func (c *Client) GetIdentity() (*UserInfo, error) {
	token, err := c.getAccessToken()
	if err != nil {
		return nil, fmt.Errorf("get OAuth token: %w", err)
	}

	identityURL := c.instanceURL + "/services/oauth2/userinfo"
	req, err := http.NewRequest(http.MethodGet, identityURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read identity response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity endpoint returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var raw struct {
		Sub      string `json:"sub"`
		Name     string `json:"name"`
		Email    string `json:"email"`
		OrgID    string `json:"organization_id"`
		UserID   string `json:"user_id"`
		Username string `json:"preferred_username"`
		UserType string `json:"user_type"`
		Active   bool   `json:"active"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal identity: %w", err)
	}

	return &UserInfo{
		UserID:      raw.UserID,
		OrgID:       raw.OrgID,
		Username:    raw.Username,
		DisplayName: raw.Name,
		Email:       raw.Email,
		UserType:    raw.UserType,
		IsActive:    raw.Active,
		InstanceURL: c.instanceURL,
	}, nil
}

// InstanceURL returns the resolved Salesforce instance URL.
func (c *Client) InstanceURL() string {
	return c.instanceURL
}
