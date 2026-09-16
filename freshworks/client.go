// Package freshworks wraps the subset of the Freshworks suite that arbetern
// surfaces to agents:
//
//   - Freshdesk (ticketing)     — https://<domain>/api/v2
//   - Freshchat (conversations) — https://<region>.freshchat.com/v2
//   - Freshworks CRM (sales)    — https://<domain>/crm/sales/api
//
// Each product authenticates differently, so the umbrella Client holds one
// optional sub-client per product; a product without credentials stays nil and
// its tools are not advertised. Freshchat and CRM are read-only; Freshdesk also
// writes a private note or a tag back onto a ticket.
package freshworks

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxResponseBody = 10 << 20 // Response body cap for io.LimitReader (10 MB).
	httpTimeout     = 30 * time.Second
	// maxSearchPages is the last page the Freshdesk filter endpoint serves.
	maxSearchPages = 10
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

// doGet performs an authenticated GET and decodes the JSON body into out.
func doGet(ctx context.Context, httpClient *http.Client, fullURL, authHeader, authValue, accept string, out any) error {
	return doJSON(ctx, httpClient, http.MethodGet, fullURL, authHeader, authValue, accept, nil, out)
}

// doJSON performs an authenticated request, sending body as JSON when non-nil,
// and decodes the JSON response into out. It maps non-2xx responses to an error
// including a snippet of the body.
func doJSON(ctx context.Context, httpClient *http.Client, method, fullURL, authHeader, authValue, accept string, body []byte, out any) error {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
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

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(respBody))
		if len(snippet) > 500 {
			snippet = snippet[:500] + "…"
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
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

func (d *DeskClient) send(ctx context.Context, method, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	return doJSON(ctx, d.httpClient, method, d.baseURL+path, "Authorization", d.authValue, "application/json", body, out)
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

// TicketSearchOpts narrows a ticket search beyond what the filter query itself
// can express.
type TicketSearchOpts struct {
	// Page is the 1-based result page; 0 selects the first page.
	Page int
	// ExcludeTags drops matched tickets carrying any of these tags. The filter
	// syntax has no negation operator, so "does not carry tag X" can only be
	// expressed over the returned results.
	ExcludeTags []string
}

// SearchTickets runs a Freshdesk ticket search query using the Freshdesk filter
// syntax, e.g. `priority:4 AND status:2`. The value is wrapped in the double
// quotes the endpoint requires. The returned total is Freshdesk's match count,
// counted before ExcludeTags is applied.
func (d *DeskClient) SearchTickets(ctx context.Context, query string, opts TicketSearchOpts) ([]Ticket, int, error) {
	if !d.Ready() {
		return nil, 0, fmt.Errorf("freshdesk not configured")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, 0, fmt.Errorf("query is required")
	}
	q := url.Values{}
	q.Set("query", `"`+query+`"`)
	if opts.Page > 0 {
		q.Set("page", clampStr(opts.Page, 1, maxSearchPages, 1))
	}
	var resp ticketSearchResponse
	if err := d.get(ctx, "/api/v2/search/tickets", q, &resp); err != nil {
		return nil, 0, err
	}
	return excludeTagged(resp.Results, opts.ExcludeTags), resp.Total, nil
}

// ListTicketFields returns the Freshdesk ticket fields (system + custom). Custom
// fields (Default=false) carry a cf_-prefixed Name usable in a ticket search as
// `cf_<name>:'value'` — this is how to scope tickets by a custom attribute such
// as "Customer Name".
func (d *DeskClient) ListTicketFields(ctx context.Context) ([]TicketField, error) {
	if !d.Ready() {
		return nil, fmt.Errorf("freshdesk not configured")
	}
	var fields []TicketField
	if err := d.get(ctx, "/api/v2/admin/ticket_fields", nil, &fields); err != nil {
		// Older/limited API scopes expose the non-admin path; fall back once.
		if err2 := d.get(ctx, "/api/v2/ticket_fields", nil, &fields); err2 != nil {
			return nil, err
		}
	}
	return fields, nil
}

// AddPrivateNote appends an internal note to a ticket. Private notes are
// visible to Freshdesk agents only and are never shown to the requester.
func (d *DeskClient) AddPrivateNote(ctx context.Context, ticketID int64, body string) (*TicketConversation, error) {
	if !d.Ready() {
		return nil, fmt.Errorf("freshdesk not configured")
	}
	if ticketID <= 0 {
		return nil, fmt.Errorf("ticket id is required")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("note body is required")
	}
	payload := map[string]any{"body": noteHTML(body), "private": true}
	var note TicketConversation
	if err := d.send(ctx, http.MethodPost, fmt.Sprintf("/api/v2/tickets/%d/notes", ticketID), payload, &note); err != nil {
		return nil, err
	}
	return &note, nil
}

// AddTags adds tags to a ticket, leaving its existing tags in place, and
// returns the resulting list along with whether the ticket was actually
// updated. An update replaces the whole tag array, so the current tags are read
// first and merged; when every tag is already present nothing is written, which
// is what makes a tag usable as an idempotency marker.
func (d *DeskClient) AddTags(ctx context.Context, ticketID int64, tags []string) ([]string, bool, error) {
	if !d.Ready() {
		return nil, false, fmt.Errorf("freshdesk not configured")
	}
	if ticketID <= 0 {
		return nil, false, fmt.Errorf("ticket id is required")
	}
	current, err := d.GetTicket(ctx, ticketID, false)
	if err != nil {
		return nil, false, err
	}
	merged, changed := mergeTags(current.Tags, tags)
	if !changed {
		return current.Tags, false, nil
	}
	var updated Ticket
	if err := d.send(ctx, http.MethodPut, fmt.Sprintf("/api/v2/tickets/%d", ticketID), map[string]any{"tags": merged}, &updated); err != nil {
		return nil, false, err
	}
	return updated.Tags, true, nil
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

// noteHTML renders plain text as the HTML body a Freshdesk note stores. The API
// keeps the body verbatim, so line breaks have to be markup or a multi-line
// note renders as one run-on paragraph.
func noteHTML(s string) string {
	return strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(s, "\r\n", "\n")), "\n", "<br />")
}

// normalizeTag folds a tag for comparison; Freshdesk treats tags
// case-insensitively.
func normalizeTag(tag string) string { return strings.ToLower(strings.TrimSpace(tag)) }

// mergeTags appends the tags not already present, keeping the existing order,
// and reports whether anything was added.
func mergeTags(existing, add []string) ([]string, bool) {
	present := make(map[string]struct{}, len(existing))
	for _, tag := range existing {
		present[normalizeTag(tag)] = struct{}{}
	}
	merged := append([]string(nil), existing...)
	changed := false
	for _, tag := range add {
		key := normalizeTag(tag)
		if key == "" {
			continue
		}
		if _, ok := present[key]; ok {
			continue
		}
		present[key] = struct{}{}
		merged = append(merged, strings.TrimSpace(tag))
		changed = true
	}
	return merged, changed
}

func excludeTagged(tickets []Ticket, tags []string) []Ticket {
	blocked := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if key := normalizeTag(tag); key != "" {
			blocked[key] = struct{}{}
		}
	}
	if len(blocked) == 0 {
		return tickets
	}
	kept := make([]Ticket, 0, len(tickets))
	for _, t := range tickets {
		skip := false
		for _, tag := range t.Tags {
			if _, ok := blocked[normalizeTag(tag)]; ok {
				skip = true
				break
			}
		}
		if !skip {
			kept = append(kept, t)
		}
	}
	return kept
}
