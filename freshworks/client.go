// Package freshworks wraps the read-only subset of the Freshworks suite that
// arbetern surfaces to agents:
//
//   - Freshdesk (ticketing)     — https://<domain>/api/v2
//   - Freshchat (conversations) — https://<region>.freshchat.com/v2
//   - Freshworks CRM (sales)    — https://<domain>/crm/sales/api
//
// Each product authenticates differently, so the umbrella Client holds one
// optional sub-client per product; a product without credentials stays nil and
// its tools are not advertised. All operations are read-only.
package freshworks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxResponseBody = 10 << 20 // Response body cap for io.LimitReader (10 MB).
	httpTimeout     = 30 * time.Second
)

// Client is the Freshworks umbrella client. Any sub-client may be nil when that
// product is not configured.
type Client struct {
	Desk *DeskClient // Freshdesk (ticketing).
	Chat *ChatClient // Freshchat (conversations).
	CRM  *CRMClient  // Freshworks CRM (sales).
}

// NewClient builds the umbrella client, constructing only the sub-clients whose
// credentials are present. Pass empty strings for products that are unused.
func NewClient(deskDomain, deskAPIKey, chatURL, chatToken, crmDomain, crmAPIKey string) *Client {
	c := &Client{}
	if strings.TrimSpace(deskDomain) != "" && strings.TrimSpace(deskAPIKey) != "" {
		c.Desk = newDeskClient(deskDomain, deskAPIKey)
	}
	if strings.TrimSpace(chatURL) != "" && strings.TrimSpace(chatToken) != "" {
		c.Chat = newChatClient(chatURL, chatToken)
	}
	if strings.TrimSpace(crmDomain) != "" && strings.TrimSpace(crmAPIKey) != "" {
		c.CRM = newCRMClient(crmDomain, crmAPIKey)
	}
	return c
}

// Ready reports whether at least one Freshworks product is configured.
func (c *Client) Ready() bool {
	if c == nil {
		return false
	}
	return c.Desk.Ready() || c.Chat.Ready() || c.CRM.Ready()
}

// Products returns the names of the configured products, for status panels and
// startup logs.
func (c *Client) Products() []string {
	if c == nil {
		return nil
	}
	var p []string
	if c.Desk.Ready() {
		p = append(p, "Freshdesk")
	}
	if c.Chat.Ready() {
		p = append(p, "Freshchat")
	}
	if c.CRM.Ready() {
		p = append(p, "Freshworks CRM")
	}
	return p
}

// doGet performs an authenticated GET and decodes the JSON body into out. It
// maps non-2xx responses to an error including a snippet of the body.
func doGet(ctx context.Context, httpClient *http.Client, fullURL, authHeader, authValue, accept string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set(authHeader, authValue)
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 500 {
			snippet = snippet[:500] + "…"
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// DeskClient talks to the Freshdesk v2 REST API. Auth is HTTP Basic with the
// API key as the username and a placeholder password ("X").
type DeskClient struct {
	baseURL    string
	authValue  string
	httpClient *http.Client
}

func newDeskClient(domain, apiKey string) *DeskClient {
	domain = strings.TrimSpace(domain)
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimRight(domain, "/")
	token := base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(apiKey) + ":X"))
	return &DeskClient{
		baseURL:    "https://" + domain,
		authValue:  "Basic " + token,
		httpClient: &http.Client{Timeout: httpTimeout},
	}
}

// Ready reports whether the Freshdesk client is configured.
func (d *DeskClient) Ready() bool { return d != nil && d.baseURL != "" && d.authValue != "" }
func (d *DeskClient) get(ctx context.Context, path string, query url.Values, out any) error {
	u := d.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return doGet(ctx, d.httpClient, u, "Authorization", d.authValue, "application/json", out)
}

// ListTickets returns recent tickets, newest-updated first. updatedSince
// (RFC3339, optional) narrows to tickets updated at or after that instant.
// requesterEmail (optional) narrows to tickets requested by that contact email.
func (d *DeskClient) ListTickets(ctx context.Context, updatedSince, requesterEmail string, page, perPage int) ([]Ticket, error) {
	if !d.Ready() {
		return nil, fmt.Errorf("freshdesk not configured")
	}
	q := url.Values{}
	q.Set("order_by", "updated_at")
	q.Set("order_type", "desc")
	// Cap the page size to what the formatter renders.
	q.Set("per_page", clampStr(perPage, 1, 50, maxListRows))
	if page > 0 {
		q.Set("page", clampStr(page, 1, 1_000_000, 1))
	}
	if s := strings.TrimSpace(updatedSince); s != "" {
		q.Set("updated_since", s)
	}
	if s := strings.TrimSpace(requesterEmail); s != "" {
		q.Set("email", s)
	}
	var tickets []Ticket
	if err := d.get(ctx, "/api/v2/tickets", q, &tickets); err != nil {
		return nil, err
	}
	return tickets, nil
}

// FindAgents resolves Freshdesk agents by email (exact, via the API's email
// filter) or by a case-insensitive substring of their name. Exactly one of
// email or name should be supplied; email is preferred. Use the returned agent
// IDs with SearchTickets (`agent_id:<id>`) to find assigned tickets.
func (d *DeskClient) FindAgents(ctx context.Context, email, name string) ([]Agent, error) {
	if !d.Ready() {
		return nil, fmt.Errorf("freshdesk not configured")
	}
	email = strings.TrimSpace(email)
	name = strings.TrimSpace(name)
	if email == "" && name == "" {
		return nil, fmt.Errorf("email or name is required")
	}
	q := url.Values{}
	if email != "" {
		q.Set("email", email)
	} else {
		// The agents endpoint has no name filter, so pull a page and match locally.
		q.Set("per_page", "100")
	}
	var agents []Agent
	if err := d.get(ctx, "/api/v2/agents", q, &agents); err != nil {
		return nil, err
	}
	if email != "" || name == "" {
		return agents, nil
	}
	needle := strings.ToLower(name)
	matched := make([]Agent, 0, len(agents))
	for _, a := range agents {
		if strings.Contains(strings.ToLower(a.Contact.Name), needle) {
			matched = append(matched, a)
		}
	}
	return matched, nil
}

// GetTicket returns a single ticket. When includeConversations is true the
// ticket's replies and notes are embedded via the `include` parameter.
func (d *DeskClient) GetTicket(ctx context.Context, ticketID int64, includeConversations bool) (*Ticket, error) {
	if !d.Ready() {
		return nil, fmt.Errorf("freshdesk not configured")
	}
	q := url.Values{}
	if includeConversations {
		q.Set("include", "conversations")
	}
	var ticket Ticket
	if err := d.get(ctx, fmt.Sprintf("/api/v2/tickets/%d", ticketID), q, &ticket); err != nil {
		return nil, err
	}
	return &ticket, nil
}

// SearchTickets runs a Freshdesk ticket search query using the Freshdesk filter
// syntax, e.g. `priority:4 AND status:2`. The value is wrapped in the double
// quotes the endpoint requires.
func (d *DeskClient) SearchTickets(ctx context.Context, query string) ([]Ticket, int, error) {
	if !d.Ready() {
		return nil, 0, fmt.Errorf("freshdesk not configured")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, 0, fmt.Errorf("query is required")
	}
	q := url.Values{}
	q.Set("query", `"`+query+`"`)
	var resp ticketSearchResponse
	if err := d.get(ctx, "/api/v2/search/tickets", q, &resp); err != nil {
		return nil, 0, err
	}
	return resp.Results, resp.Total, nil
}

// ChatClient talks to the Freshchat v2 API. Auth is a Bearer JWT.
type ChatClient struct {
	baseURL    string // includes the trailing /v2
	authValue  string
	httpClient *http.Client
}

func newChatClient(baseURL, token string) *ChatClient {
	return &ChatClient{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		authValue:  "Bearer " + strings.TrimSpace(token),
		httpClient: &http.Client{Timeout: httpTimeout},
	}
}

// Ready reports whether the Freshchat client is configured.
func (c *ChatClient) Ready() bool {
	return c != nil && c.baseURL != "" && c.authValue != "Bearer "
}

func (c *ChatClient) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return doGet(ctx, c.httpClient, u, "Authorization", c.authValue, "application/json", out)
}

// GetConversation returns the header for a Freshchat conversation by ID.
func (c *ChatClient) GetConversation(ctx context.Context, conversationID string) (*ChatConversation, error) {
	if !c.Ready() {
		return nil, fmt.Errorf("freshchat not configured")
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil, fmt.Errorf("conversation_id is required")
	}
	var conv ChatConversation
	if err := c.get(ctx, "/conversations/"+url.PathEscape(conversationID), nil, &conv); err != nil {
		return nil, err
	}
	return &conv, nil
}

// GetConversationMessages returns the messages in a Freshchat conversation.
// page is 1-based; pass 0 for the first page.
func (c *ChatClient) GetConversationMessages(ctx context.Context, conversationID string, page int) ([]ChatMessage, error) {
	if !c.Ready() {
		return nil, fmt.Errorf("freshchat not configured")
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil, fmt.Errorf("conversation_id is required")
	}
	q := url.Values{}
	q.Set("items_per_page", clampStr(0, 1, maxChatMessages, maxChatMessages))
	if page > 0 {
		q.Set("page", clampStr(page, 1, 1_000_000, 1))
	}
	var resp chatMessagesResponse
	if err := c.get(ctx, "/conversations/"+url.PathEscape(conversationID)+"/messages", q, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// CRMClient talks to the Freshworks CRM (Freshsales) API. Auth is the
// `Authorization: Token token=<key>` header.
type CRMClient struct {
	baseURL    string
	authValue  string
	httpClient *http.Client
}

func newCRMClient(domain, apiKey string) *CRMClient {
	domain = strings.TrimSpace(domain)
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimRight(domain, "/")
	// Tolerate callers that pass the full ".../crm/sales" UI URL.
	domain = strings.TrimSuffix(domain, "/crm/sales")
	return &CRMClient{
		baseURL:    "https://" + domain + "/crm/sales/api",
		authValue:  "Token token=" + strings.TrimSpace(apiKey),
		httpClient: &http.Client{Timeout: httpTimeout},
	}
}

// Ready reports whether the CRM client is configured.
func (c *CRMClient) Ready() bool {
	return c != nil && c.baseURL != "" && c.authValue != "Token token="
}

func (c *CRMClient) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return doGet(ctx, c.httpClient, u, "Authorization", c.authValue, "application/json", out)
}

// Search runs the CRM global search. entities optionally narrows the searched
// record types (contact, sales_account, deal, user, lead); empty defaults to
// contacts, deals and accounts.
func (c *CRMClient) Search(ctx context.Context, query string, entities []string) ([]CRMSearchResult, error) {
	if !c.Ready() {
		return nil, fmt.Errorf("freshworks crm not configured")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	clean := make([]string, 0, len(entities))
	for _, e := range entities {
		if e = strings.TrimSpace(e); e != "" {
			clean = append(clean, e)
		}
	}
	if len(clean) == 0 {
		clean = []string{"contact", "deal", "sales_account"}
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("include", strings.Join(clean, ","))
	// Decode into raw first: the search endpoint normally returns a bare array,
	// but some accounts wrap the results in an object keyed by entity type.
	var raw json.RawMessage
	if err := c.get(ctx, "/search", q, &raw); err != nil {
		return nil, err
	}
	return parseCRMSearch(raw)
}

// parseCRMSearch decodes a CRM search body that may be either a bare array of
// results or an object whose values are arrays of results.
func parseCRMSearch(raw json.RawMessage) ([]CRMSearchResult, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var results []CRMSearchResult
		if err := json.Unmarshal(raw, &results); err != nil {
			return nil, fmt.Errorf("decoding search results: %w", err)
		}
		return results, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("decoding search results: %w", err)
	}
	var results []CRMSearchResult
	for _, v := range obj {
		var part []CRMSearchResult
		if json.Unmarshal(v, &part) == nil {
			results = append(results, part...)
		}
	}
	return results, nil
}

// GetContact returns a CRM contact by ID.
func (c *CRMClient) GetContact(ctx context.Context, contactID int64) (*CRMContact, error) {
	if !c.Ready() {
		return nil, fmt.Errorf("freshworks crm not configured")
	}
	var resp contactResponse
	if err := c.get(ctx, fmt.Sprintf("/contacts/%d", contactID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp.Contact, nil
}

// GetDeal returns a CRM deal by ID.
func (c *CRMClient) GetDeal(ctx context.Context, dealID int64) (*CRMDeal, error) {
	if !c.Ready() {
		return nil, fmt.Errorf("freshworks crm not configured")
	}
	var resp dealResponse
	if err := c.get(ctx, fmt.Sprintf("/deals/%d", dealID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp.Deal, nil
}

// clampStr clamps n to [lo, hi], substituting def when n <= 0, and returns it
// as a decimal string for use as a query value.
func clampStr(n, lo, hi, def int) string {
	if n <= 0 {
		n = def
	}
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	return fmt.Sprintf("%d", n)
}
