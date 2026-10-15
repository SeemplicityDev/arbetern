package atlassian

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Jira REST API v3

// PermissionGrant describes whether a specific Jira permission is granted.
type PermissionGrant struct {
	Key            string `json:"key"`
	HavePermission bool   `json:"havePermission"`
}

// GetMyPermissions queries the Jira REST API /mypermissions endpoint and returns
// which of the requested permission keys the authenticated user actually has.
func (c *Client) GetMyPermissions(keys []string) (map[string]bool, error) {
	qs := url.Values{"permissions": {strings.Join(keys, ",")}}
	endpoint := fmt.Sprintf("%s/rest/api/3/mypermissions?%s", c.baseURL, qs.Encode())

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build mypermissions request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mypermissions request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return nil, fmt.Errorf("mypermissions returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Permissions map[string]struct {
			HavePermission bool `json:"havePermission"`
		} `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode mypermissions: %w", err)
	}

	grants := make(map[string]bool, len(result.Permissions))
	for k, p := range result.Permissions {
		grants[k] = p.HavePermission
	}
	return grants, nil
}

// Issue represents a created Jira issue.
type Issue struct {
	Key    string `json:"key"`
	ID     string `json:"id"`
	Self   string `json:"self"`
	Browse string `json:"-"` // human-friendly URL
}

// CreateIssueInput holds parameters for creating a Jira issue.
type CreateIssueInput struct {
	Project     string // project key, e.g. "ENG"
	Summary     string
	Description string
	IssueType   string // e.g. "Task", "Bug", "Story"
	Labels      []string
	AssigneeID  string // Jira account ID of the assignee (optional)
	// ReporterID is the Jira account ID of the human who originated the
	// request (optional). When set, the created issue records Reporter =
	// that person for auditability/compliance instead of the service account
	// that authenticates against the API. Leave empty for headless/automation
	// runs so Jira defaults the reporter to the bot/service account.
	ReporterID string
	// Fields is an escape hatch for arbitrary Jira create-issue fields
	// (e.g. "priority": {"name":"High"}, "components": [{"name":"API"}],
	// or any "customfield_XXXXX"). Values are merged into the request
	// payload's "fields" object. Typed fields above (Summary, Description,
	// IssueType, Labels, AssigneeID, Project) always win on conflict so a
	// caller cannot silently drop them by passing the same key here.
	Fields map[string]interface{}
}

// createIssuePayload is the JSON body sent to the Jira API.
type createIssuePayload struct {
	Fields createIssueFields `json:"fields"`
}

type createIssueFields struct {
	Project   projectRef `json:"project"`
	Summary   string     `json:"summary"`
	IssueType issueType  `json:"issuetype"`
	// Description uses Atlassian Document Format (ADF).
	Description *adfDoc     `json:"description,omitempty"`
	Labels      []string    `json:"labels,omitempty"`
	Assignee    *accountRef `json:"assignee,omitempty"`
	Reporter    *accountRef `json:"reporter,omitempty"`
}

type accountRef struct {
	AccountID string `json:"accountId"`
}

type projectRef struct {
	Key string `json:"key"`
}

type issueType struct {
	Name string `json:"name"`
}

// --- Atlassian Document Format (ADF) helpers ---

type adfDoc struct {
	Type    string    `json:"type"`
	Version int       `json:"version"`
	Content []adfNode `json:"content"`
}

type adfNode struct {
	Type    string          `json:"type"`
	Attrs   *adfAttrs       `json:"attrs,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

type adfAttrs struct {
	Level    int    `json:"level,omitempty"`
	Order    int    `json:"order,omitempty"`
	URL      string `json:"url,omitempty"`
	Href     string `json:"href,omitempty"`
	Language string `json:"language,omitempty"`
	// Mention attrs: ADF mention nodes carry the user's accountId in `id`
	// and the visible @-prefixed display name in `text`.
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
}

type adfInline struct {
	Type  string    `json:"type"`
	Text  string    `json:"text,omitempty"`
	Marks []adfMark `json:"marks,omitempty"`
	Attrs *adfAttrs `json:"attrs,omitempty"`
}

type adfMark struct {
	Type  string        `json:"type"`
	Attrs *adfMarkAttrs `json:"attrs,omitempty"`
}

type adfMarkAttrs struct {
	Href string `json:"href,omitempty"`
}

// marshalInlines converts inline elements to json.RawMessage.
// Empty text nodes are silently dropped — they violate the ADF specification
// and cause Jira to reject the payload with HTTP 400 INVALID_INPUT.
// Mention nodes are always retained even when their Text field is empty,
// since their content lives in the attrs (id/text) instead.
func marshalInlines(inlines []adfInline) json.RawMessage {
	var filtered []adfInline
	for _, n := range inlines {
		if n.Type == "text" && n.Text == "" {
			continue
		}
		filtered = append(filtered, n)
	}
	if len(filtered) == 0 {
		return nil
	}
	b, _ := json.Marshal(filtered)
	return b
}

// marshalNodes converts node elements to json.RawMessage.
func marshalNodes(nodes []adfNode) json.RawMessage {
	if len(nodes) == 0 {
		return nil
	}
	b, _ := json.Marshal(nodes)
	return b
}

// textToADF converts markdown-like text into a proper Atlassian Document Format document.
// Supports: # headings, - bullet lists, 1) ordered lists, **bold**, `code`, and plain paragraphs.
func textToADF(text string) *adfDoc {
	if text == "" {
		return nil
	}

	lines := strings.Split(text, "\n")
	var nodes []adfNode
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Skip empty lines.
		if trimmed == "" {
			i++
			continue
		}

		// Horizontal rule: ---
		if trimmed == "---" || trimmed == "***" || trimmed == "___" {
			nodes = append(nodes, adfNode{Type: "rule"})
			i++
			continue
		}

		// Fenced code block: ```lang ... ```
		if strings.HasPrefix(trimmed, "```") {
			lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			i++
			var codeLines []string
			for i < len(lines) {
				if strings.TrimSpace(lines[i]) == "```" {
					i++
					break
				}
				codeLines = append(codeLines, lines[i])
				i++
			}
			codeText := strings.Join(codeLines, "\n")
			if codeText != "" {
				attrs := &adfAttrs{}
				if lang != "" {
					attrs.Language = lang
				}
				nodes = append(nodes, adfNode{
					Type:    "codeBlock",
					Attrs:   attrs,
					Content: marshalInlines([]adfInline{{Type: "text", Text: codeText}}),
				})
			}
			continue
		}

		// Block quote: > text
		if strings.HasPrefix(trimmed, "> ") {
			var quoteLines []string
			for i < len(lines) {
				lt := strings.TrimSpace(lines[i])
				if !strings.HasPrefix(lt, "> ") {
					break
				}
				quoteLines = append(quoteLines, strings.TrimPrefix(lt, "> "))
				i++
			}
			quoteText := strings.Join(quoteLines, " ")
			nodes = append(nodes, adfNode{
				Type: "blockquote",
				Content: marshalNodes([]adfNode{
					{Type: "paragraph", Content: marshalInlines(parseInlineMarkdown(quoteText))},
				}),
			})
			continue
		}

		// Headings: # ## ### etc.
		if strings.HasPrefix(trimmed, "#") {
			level := 0
			for _, c := range trimmed {
				if c == '#' {
					level++
				} else {
					break
				}
			}
			if level > 6 {
				level = 6
			}
			headingText := strings.TrimSpace(trimmed[level:])
			if headingText != "" {
				nodes = append(nodes, adfNode{
					Type:    "heading",
					Attrs:   &adfAttrs{Level: level},
					Content: marshalInlines(parseInlineMarkdown(headingText)),
				})
			}
			i++
			continue
		}

		// Bullet list: lines starting with - or *
		if isBulletLine(trimmed) {
			var items []adfNode
			for i < len(lines) {
				lt := strings.TrimSpace(lines[i])
				if !isBulletLine(lt) {
					break
				}
				itemText := strings.TrimSpace(lt[1:]) // strip - or *
				items = append(items, adfNode{
					Type: "listItem",
					Content: marshalNodes([]adfNode{
						{Type: "paragraph", Content: marshalInlines(parseInlineMarkdown(itemText))},
					}),
				})
				i++
			}
			nodes = append(nodes, adfNode{
				Type:    "bulletList",
				Content: marshalNodes(items),
			})
			continue
		}

		// Ordered list: lines starting with number) or number.
		if isOrderedLine(trimmed) {
			var items []adfNode
			for i < len(lines) {
				lt := strings.TrimSpace(lines[i])
				if !isOrderedLine(lt) {
					break
				}
				itemText := stripOrderedPrefix(lt)
				items = append(items, adfNode{
					Type: "listItem",
					Content: marshalNodes([]adfNode{
						{Type: "paragraph", Content: marshalInlines(parseInlineMarkdown(itemText))},
					}),
				})
				i++
			}
			nodes = append(nodes, adfNode{
				Type:    "orderedList",
				Content: marshalNodes(items),
			})
			continue
		}

		// Regular paragraph — collect consecutive non-special lines.
		var paraLines []string
		for i < len(lines) {
			lt := strings.TrimSpace(lines[i])
			if lt == "" || strings.HasPrefix(lt, "#") || isBulletLine(lt) || isOrderedLine(lt) {
				break
			}
			paraLines = append(paraLines, lt)
			i++
		}
		paraText := strings.Join(paraLines, " ")
		nodes = append(nodes, adfNode{
			Type:    "paragraph",
			Content: marshalInlines(parseInlineMarkdown(paraText)),
		})
	}

	if len(nodes) == 0 {
		return nil
	}
	return &adfDoc{
		Type:    "doc",
		Version: 1,
		Content: nodes,
	}
}

func isBulletLine(s string) bool {
	return (strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ")) && len(s) > 2
}

var orderedLineRe = regexp.MustCompile(`^\d+[.)]\s+`)

func isOrderedLine(s string) bool {
	return orderedLineRe.MatchString(s)
}

func stripOrderedPrefix(s string) string {
	loc := orderedLineRe.FindStringIndex(s)
	if loc == nil {
		return s
	}
	return strings.TrimSpace(s[loc[1]:])
}

// parseInlineMarkdown converts simple inline markdown (**bold**, `code`, [text](url),
// and @[Display Name](accountId) Jira mentions) to ADF inlines.
func parseInlineMarkdown(text string) []adfInline {
	var inlines []adfInline
	remaining := text
	for len(remaining) > 0 {
		// Find the next special marker.
		boldIdx := strings.Index(remaining, "**")
		codeIdx := strings.Index(remaining, "`")
		linkIdx := strings.Index(remaining, "[")
		mentionIdx := strings.Index(remaining, "@[")

		// No more markers.
		if boldIdx < 0 && codeIdx < 0 && linkIdx < 0 && mentionIdx < 0 {
			if remaining != "" {
				inlines = append(inlines, adfInline{Type: "text", Text: remaining})
			}
			break
		}

		// Determine which comes first.
		type candidate struct {
			idx    int
			marker string
		}
		candidates := []candidate{}
		if mentionIdx >= 0 {
			candidates = append(candidates, candidate{mentionIdx, "@["})
		}
		if boldIdx >= 0 {
			candidates = append(candidates, candidate{boldIdx, "**"})
		}
		if codeIdx >= 0 {
			candidates = append(candidates, candidate{codeIdx, "`"})
		}
		// Skip the plain link marker when it is the `[` that belongs to a
		// `@[...]` mention — the mention candidate handles it.
		if linkIdx >= 0 && (mentionIdx < 0 || linkIdx != mentionIdx+1) {
			candidates = append(candidates, candidate{linkIdx, "["})
		}
		// Pick the earliest.
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.idx < best.idx {
				best = c
			}
		}
		nextIdx := best.idx
		marker := best.marker

		// Add plain text before the marker.
		if nextIdx > 0 {
			inlines = append(inlines, adfInline{Type: "text", Text: remaining[:nextIdx]})
		}

		// Handle Jira mention: @[Display Name](accountId)
		if marker == "@[" {
			rest := remaining[nextIdx+2:]
			closeB := strings.Index(rest, "](")
			if closeB >= 0 {
				displayName := rest[:closeB]
				idStart := rest[closeB+2:]
				closeP := strings.Index(idStart, ")")
				if closeP >= 0 {
					accountID := strings.TrimSpace(idStart[:closeP])
					if accountID != "" && displayName != "" {
						inlines = append(inlines, adfInline{
							Type: "mention",
							Attrs: &adfAttrs{
								ID:   accountID,
								Text: "@" + displayName,
							},
						})
						remaining = idStart[closeP+1:]
						continue
					}
				}
			}
			// Not a valid mention — treat `@` as plain text and continue
			// parsing from the `[` (which may still be a markdown link).
			inlines = append(inlines, adfInline{Type: "text", Text: "@"})
			remaining = remaining[nextIdx+1:]
			continue
		}

		// Handle markdown link: [text](url)
		if marker == "[" {
			rest := remaining[nextIdx+1:]
			closeB := strings.Index(rest, "](")
			if closeB >= 0 {
				linkText := rest[:closeB]
				urlStart := rest[closeB+2:]
				closeP := strings.Index(urlStart, ")")
				if closeP >= 0 {
					href := urlStart[:closeP]
					inlines = append(inlines, adfInline{
						Type:  "text",
						Text:  linkText,
						Marks: []adfMark{{Type: "link", Attrs: &adfMarkAttrs{Href: href}}},
					})
					remaining = urlStart[closeP+1:]
					continue
				}
			}
			// Not a valid link — treat [ as plain text.
			inlines = append(inlines, adfInline{Type: "text", Text: "["})
			remaining = rest
			continue
		}

		// Find the closing marker for bold/code.
		rest := remaining[nextIdx+len(marker):]
		closeIdx := strings.Index(rest, marker)
		if closeIdx < 0 {
			// No closing marker — treat as plain text.
			inlines = append(inlines, adfInline{Type: "text", Text: remaining[nextIdx:]})
			break
		}

		inner := rest[:closeIdx]
		if marker == "**" {
			inlines = append(inlines, adfInline{
				Type:  "text",
				Text:  inner,
				Marks: []adfMark{{Type: "strong"}},
			})
		} else {
			inlines = append(inlines, adfInline{
				Type:  "text",
				Text:  inner,
				Marks: []adfMark{{Type: "code"}},
			})
		}

		remaining = rest[closeIdx+len(marker):]
	}
	return inlines
}

// CreateIssue creates a new issue in Jira and returns its details.
func (c *Client) CreateIssue(input CreateIssueInput) (*Issue, error) {
	// Always use the configured default project when set — prevents the
	// LLM from accidentally creating tickets in the wrong project.
	if c.projectKey != "" {
		input.Project = c.projectKey
	}
	return c.createIssue(input)
}

// CreateIssueInProject creates an issue in input.Project, which configuration chose rather than a model.
func (c *Client) CreateIssueInProject(input CreateIssueInput) (*Issue, error) {
	return c.createIssue(input)
}

func (c *Client) createIssue(input CreateIssueInput) (*Issue, error) {
	if input.Project == "" {
		return nil, fmt.Errorf("project key is required (no default configured)")
	}
	if input.IssueType == "" {
		input.IssueType = "Task"
	}

	payload := createIssuePayload{
		Fields: createIssueFields{
			Project:     projectRef{Key: input.Project},
			Summary:     input.Summary,
			IssueType:   issueType{Name: input.IssueType},
			Description: textToADF(input.Description),
			Labels:      input.Labels,
		},
	}
	if input.AssigneeID != "" {
		payload.Fields.Assignee = &accountRef{AccountID: input.AssigneeID}
	}
	if input.ReporterID != "" {
		payload.Fields.Reporter = &accountRef{AccountID: input.ReporterID}
	}

	body, err := marshalCreateIssueBody(payload, input.Fields)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s/rest/api/3/issue", c.baseURL)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, fmt.Errorf("auth request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to parse structured Jira error for actionable diagnostics.
		var jiraErr struct {
			ErrorMessages []string          `json:"errorMessages"`
			Errors        map[string]string `json:"errors"`
		}
		if json.Unmarshal(respBody, &jiraErr) == nil {
			var parts []string
			parts = append(parts, jiraErr.ErrorMessages...)
			for field, msg := range jiraErr.Errors {
				parts = append(parts, fmt.Sprintf("%s: %s", field, msg))
			}
			if len(parts) > 0 {
				return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, strings.Join(parts, "; "))
			}
		}
		return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var issue Issue
	if err := json.Unmarshal(respBody, &issue); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	issue.Browse = fmt.Sprintf("%s/browse/%s", c.siteURL, issue.Key)
	return &issue, nil
}

// ListProjects returns the keys of all projects visible to the authenticated user.
// Paginates through all pages using startAt/maxResults offset pagination.
func (c *Client) ListProjects() ([]string, error) {
	var allKeys []string
	startAt := 0

	for {
		reqURL := fmt.Sprintf("%s/rest/api/3/project/search?maxResults=%d&startAt=%d&status=live",
			c.baseURL, jiraPageSize, startAt)
		req, err := http.NewRequest(http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.authRequest(req); err != nil {
			return nil, fmt.Errorf("auth request: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("send request: %w", err)
		}

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
		}

		var result struct {
			Values []struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"values"`
			IsLast bool `json:"isLast"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}

		for _, p := range result.Values {
			allKeys = append(allKeys, fmt.Sprintf("%s (%s)", p.Key, p.Name))
		}

		if result.IsLast || len(result.Values) == 0 {
			break
		}
		startAt += len(result.Values)
	}

	return allKeys, nil
}

// JiraUser represents a user returned by the Jira user search API.
type JiraUser struct {
	AccountID    string            `json:"accountId"`
	DisplayName  string            `json:"displayName"`
	EmailAddress string            `json:"emailAddress,omitempty"`
	AvatarURLs   map[string]string `json:"avatarUrls,omitempty"`
	Active       bool              `json:"active"`
}

// SearchUsers searches for Jira users matching the given query string.
// It first tries the project-scoped assignable-user endpoint (better results for
// teams/service-accounts), then falls back to the general user search.
// Results are ranked by how well the display name matches the query.
func (c *Client) SearchUsers(query string) ([]JiraUser, error) {
	users, err := c.SearchAssignableUsers(query, c.projectKey)
	if err != nil || len(users) == 0 {
		return c.SearchUsersGeneral(query)
	}
	return users, nil
}

// SearchUsersGeneral searches for Jira users using the general /user/search endpoint
// which does NOT require project access. Use this when you only need the user's account ID
// (e.g., for JQL queries) and don't need to verify project-assignability.
func (c *Client) SearchUsersGeneral(query string) ([]JiraUser, error) {
	return c.searchUsersRaw(query, "")
}

// searchUsersRaw performs a single user search API call and returns active users.
func (c *Client) searchUsersRaw(query, project string) ([]JiraUser, error) {
	var searchURL string
	if project != "" {
		searchURL = fmt.Sprintf("%s/rest/api/3/user/assignable/search?query=%s&project=%s&maxResults=50",
			c.baseURL, url.QueryEscape(query), url.QueryEscape(project))
	} else {
		searchURL = fmt.Sprintf("%s/rest/api/3/user/search?query=%s&maxResults=50",
			c.baseURL, url.QueryEscape(query))
	}

	req, err := http.NewRequest(http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, fmt.Errorf("auth request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var users []JiraUser
	if err := json.Unmarshal(respBody, &users); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	active := make([]JiraUser, 0, len(users))
	for _, u := range users {
		if u.Active {
			active = append(active, u)
		}
	}
	return active, nil
}

// coreQuery strips common suffixes like "team" or "group" from the query.
func coreQuery(query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	for _, suffix := range []string{" team", " group", " squad"} {
		q = strings.TrimSuffix(q, suffix)
	}
	return strings.TrimSpace(q)
}

// SearchAssignableUsers searches for users assignable to the given project.
// It searches with both the original query and the core term (e.g. "application"
// from "application team") to avoid Jira's fuzzy matching on common words like "team".
// Results are deduplicated, ranked by display-name match quality, and returned.
func (c *Client) SearchAssignableUsers(query, project string) ([]JiraUser, error) {
	core := coreQuery(query)

	// Always search with the core term first (most likely to find the right match).
	users, err := c.searchUsersRaw(core, project)
	if err != nil {
		return nil, err
	}

	// If the core differs from the original query, also search with the full query
	// and merge results (deduplicating by account ID).
	if core != strings.ToLower(strings.TrimSpace(query)) {
		extra, err := c.searchUsersRaw(query, project)
		if err == nil && len(extra) > 0 {
			seen := make(map[string]bool, len(users))
			for _, u := range users {
				seen[u.AccountID] = true
			}
			for _, u := range extra {
				if !seen[u.AccountID] {
					users = append(users, u)
				}
			}
		}
	}

	// Rank by display-name match quality.
	ranked := rankUsersByMatch(users, query)
	return ranked, nil
}

// rankUsersByMatch sorts users so that the best display-name match for query
// comes first. Priority: exact match > starts-with > contains > rest.
func rankUsersByMatch(users []JiraUser, query string) []JiraUser {
	if len(users) <= 1 {
		return users
	}

	q := strings.ToLower(strings.TrimSpace(query))
	// Strip common suffixes like "team" for matching, e.g. "application team" → "application"
	qCore := q
	for _, suffix := range []string{" team", " group"} {
		qCore = strings.TrimSuffix(qCore, suffix)
	}
	qCore = strings.TrimSpace(qCore)

	type scored struct {
		user  JiraUser
		score int // lower is better
	}
	scored_users := make([]scored, len(users))
	for i, u := range users {
		dn := strings.ToLower(u.DisplayName)

		score := 100 // default: no match bonus
		switch {
		case dn == q || dn == qCore:
			score = 0 // exact match
		case strings.HasPrefix(dn, qCore):
			score = 10 // starts with core query
		case strings.Contains(dn, qCore):
			score = 20 // contains core query
		case strings.HasPrefix(dn, q):
			score = 30
		case strings.Contains(dn, q):
			score = 40
		}
		scored_users[i] = scored{user: u, score: score}
	}

	// Sort by score ascending (best match first).
	for i := 1; i < len(scored_users); i++ {
		for j := i; j > 0 && scored_users[j].score < scored_users[j-1].score; j-- {
			scored_users[j], scored_users[j-1] = scored_users[j-1], scored_users[j]
		}
	}

	result := make([]JiraUser, len(scored_users))
	for i, s := range scored_users {
		result[i] = s.user
	}
	return result
}

// BestUserMatch returns the best-matching user and whether the match is good.
// A match is considered good if the user's display name contains the core query term.
func BestUserMatch(users []JiraUser, query string) (JiraUser, bool) {
	if len(users) == 0 {
		return JiraUser{}, false
	}
	ranked := rankUsersByMatch(users, query)
	core := coreQuery(query)
	dn := strings.ToLower(ranked[0].DisplayName)
	q := strings.ToLower(strings.TrimSpace(query))
	return ranked[0], strings.Contains(dn, core) || strings.Contains(dn, q)
}

// TeamFieldInfo stores discovered team field metadata.
type TeamFieldInfo struct {
	ID      string // e.g. "customfield_10001"
	JQLName string // e.g. "Team[Team]" or "cf[10001]"
}

// FindTeamFields discovers all "Team"-like custom fields from Jira field metadata.
// Results are sorted so that the actual Jira Teams field (clause "Team[Team]") comes first,
// followed by dropdown/select variants.
func (c *Client) FindTeamFields() ([]TeamFieldInfo, error) {
	reqURL := fmt.Sprintf("%s/rest/api/3/field", c.baseURL)
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, fmt.Errorf("auth request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var fields []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		ClauseNames []string `json:"clauseNames"`
		Schema      *struct {
			Type   string `json:"type"`
			Custom string `json:"custom"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(respBody, &fields); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	// Collect all team-like fields, partition by priority.
	var preferred []TeamFieldInfo // fields with [Team] clause (actual Jira Teams)
	var fallback []TeamFieldInfo  // other team-named fields (dropdown, select, etc.)

	for _, f := range fields {
		if f.Schema == nil {
			continue
		}
		nameL := strings.ToLower(f.Name)
		customL := strings.ToLower(f.Schema.Custom)
		if nameL == "team" || strings.Contains(customL, "teams") {
			jqlName := f.ID
			if len(f.ClauseNames) > 0 {
				jqlName = f.ClauseNames[0]
			}
			info := TeamFieldInfo{ID: f.ID, JQLName: jqlName}

			// Check if any clause name contains "[Team]" — that's the real Teams integration field.
			isTeamsField := false
			for _, cn := range f.ClauseNames {
				if strings.Contains(cn, "[Team]") {
					isTeamsField = true
					info.JQLName = cn // prefer the [Team] clause name
					break
				}
			}
			if isTeamsField || strings.Contains(customL, "teams") {
				preferred = append(preferred, info)
			} else {
				fallback = append(fallback, info)
			}
		}
	}

	// Preferred fields first, then fallback.
	result := append(preferred, fallback...)
	if len(result) == 0 {
		return nil, fmt.Errorf("no Team custom field found")
	}
	return result, nil
}

type jiraField struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	ClauseNames []string `json:"clauseNames"`
	Schema      *struct {
		Type   string `json:"type"`
		Custom string `json:"custom"`
	} `json:"schema"`
}

func (c *Client) fetchFields() ([]jiraField, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/rest/api/3/field", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira API error (HTTP %d)", resp.StatusCode)
	}
	var fields []jiraField
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// discoverExtraFields returns the custom field IDs for Team and Sprint, found on first use.
// A failed discovery is retried after fieldCatalogRetry, so one bad answer does not hide the fields for the process's life.
func (c *Client) discoverExtraFields() (team, sprint string) {
	c.fieldsMu.Lock()
	defer c.fieldsMu.Unlock()
	if c.extraFieldsFound || time.Since(c.extraFieldsTried) < fieldCatalogRetry {
		return c.teamFieldID, c.sprintFieldID
	}
	c.extraFieldsTried = time.Now()
	fields, err := c.fetchFields()
	if err != nil {
		// Stamped again after the attempt, so callers queued behind a slow failure reuse it instead of retrying in turn.
		c.extraFieldsTried = time.Now()
		return c.teamFieldID, c.sprintFieldID
	}
	for _, f := range fields {
		nameL := strings.ToLower(f.Name)
		if f.Schema != nil {
			customL := strings.ToLower(f.Schema.Custom)
			// Team field
			if c.teamFieldID == "" && (nameL == "team" || strings.Contains(customL, "teams")) {
				c.teamFieldID = f.ID
			}
			// Sprint field
			if c.sprintFieldID == "" && nameL == "sprint" {
				c.sprintFieldID = f.ID
			}
		}
		if c.teamFieldID != "" && c.sprintFieldID != "" {
			break
		}
	}
	c.extraFieldsFound = true
	return c.teamFieldID, c.sprintFieldID
}

// extraFieldIDs returns the custom field IDs to append to search queries.
func (c *Client) extraFieldIDs() []string {
	team, sprint := c.discoverExtraFields()
	var ids []string
	if team != "" {
		ids = append(ids, team)
	}
	if sprint != "" {
		ids = append(ids, sprint)
	}
	return ids
}

// extractSprintName extracts the active sprint name from a raw sprint field value.
func extractSprintName(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// Sprint is usually an array of sprint objects; pick the last active one.
	var sprints []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &sprints); err == nil && len(sprints) > 0 {
		// Prefer active sprint, fall back to last one.
		for i := len(sprints) - 1; i >= 0; i-- {
			if strings.EqualFold(sprints[i].State, "active") {
				return sprints[i].Name
			}
		}
		return sprints[len(sprints)-1].Name
	}
	// Single sprint object.
	var single struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &single); err == nil && single.Name != "" {
		return single.Name
	}
	return ""
}

// extractCustomFields extracts Team and Sprint names from a raw JSON fields object
// using the cached custom field IDs.
func (c *Client) extractCustomFields(rawFields json.RawMessage) (team, sprint string) {
	if len(rawFields) == 0 {
		return "", ""
	}
	var allFields map[string]json.RawMessage
	if err := json.Unmarshal(rawFields, &allFields); err != nil {
		return "", ""
	}
	teamID, sprintID := c.discoverExtraFields()
	if teamID != "" {
		if v, ok := allFields[teamID]; ok {
			team = extractTeamName(v)
		}
	}
	if sprintID != "" {
		if v, ok := allFields[sprintID]; ok {
			sprint = extractSprintName(v)
		}
	}
	return team, sprint
}

// ResolveTeam attempts to find a team by name.
// It discovers all team-like fields and tries multiple strategies for each.
// Returns (fieldID, teamID, displayName, error).
func (c *Client) ResolveTeam(teamName string) (string, string, string, error) {
	fields, err := c.FindTeamFields()
	if err != nil {
		return "", "", "", fmt.Errorf("find team fields: %w", err)
	}

	core := coreQuery(teamName)

	// --- Strategy 1: Jira Teams REST API (field-independent) ---
	if team, err := c.searchTeamsAPI(core); err == nil && team != nil {
		return fields[0].ID, team.teamID, team.displayName, nil
	}

	// --- Strategy 2: For each discovered team field, scan existing issues ---
	for _, field := range fields {
		teamFromIssues, err := c.findTeamFromExistingIssues(&field, core)
		if err == nil && teamFromIssues != nil {
			return field.ID, teamFromIssues.teamID, teamFromIssues.displayName, nil
		}
	}

	return "", "", "", fmt.Errorf("no team found matching %q (tried Teams API and issue-scan across %d fields)", teamName, len(fields))
}

// SetTeamField tries multiple value formats to set the team custom field on an issue.
// Atlassian Team fields accept different formats depending on the Jira/plugin version.
func (c *Client) SetTeamField(issueKey, fieldID, teamID string) error {
	// Try these formats in order — different Jira versions accept different ones.
	formats := []struct {
		name  string
		value interface{}
	}{
		{"bare ID", teamID},
		{"id object", map[string]string{"id": teamID}},
		{"teamId object", map[string]string{"teamId": teamID}},
	}

	var lastErr error
	for _, f := range formats {
		err := c.UpdateIssueFields(issueKey, map[string]interface{}{
			fieldID: f.value,
		})
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// findTeamFromExistingIssues searches for issues in the default project that have the
// Team field set, then filters client-side for a matching team name.
func (c *Client) findTeamFromExistingIssues(field *TeamFieldInfo, query string) (*teamSearchResult, error) {
	// The Team custom field in Jira uses the clause name "Team[Team]" in JQL.
	// We try: the discovered JQL clause name (e.g. "Team[Team]"), then cf[XXXX] as fallback.
	jqlClause := fmt.Sprintf(`"%s"`, field.JQLName)
	cfFallback := fmt.Sprintf("cf[%s]", strings.TrimPrefix(field.ID, "customfield_"))

	jqlQueries := []string{
		// Try with the proper JQL clause name first (e.g. "Team[Team]").
		fmt.Sprintf(`project = %s AND %s IS NOT EMPTY ORDER BY updated DESC`, c.projectKey, jqlClause),
		// Fallback: cf[XXXX] syntax, project-scoped.
		fmt.Sprintf(`project = %s AND %s IS NOT EMPTY ORDER BY updated DESC`, c.projectKey, cfFallback),
		// Fallback: any issue with the team field set.
		fmt.Sprintf(`%s IS NOT EMPTY ORDER BY updated DESC`, jqlClause),
	}

	q := strings.ToLower(query)

	for _, jql := range jqlQueries {
		searchURL := fmt.Sprintf("%s/rest/api/3/search/jql?jql=%s&maxResults=50&fields=%s",
			c.baseURL, url.QueryEscape(jql), field.ID)

		req, err := http.NewRequest(http.MethodGet, searchURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.authRequest(req); err != nil {
			continue
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			continue
		}

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}

		var result struct {
			Total  int `json:"total"`
			Issues []struct {
				Key    string                     `json:"key"`
				Fields map[string]json.RawMessage `json:"fields"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			continue
		}

		// Scan returned issues for a team field value matching our query.
		for _, issue := range result.Issues {
			teamValue, ok := issue.Fields[field.ID]
			if !ok || string(teamValue) == "null" {
				continue
			}

			// Try to extract the team display name from different possible structures.
			name := extractTeamName(teamValue)
			if name == "" {
				continue
			}

			nameL := strings.ToLower(name)
			if nameL == q || strings.Contains(nameL, q) || strings.Contains(q, nameL) {
				teamID := extractTeamID(teamValue)
				if teamID == "" {
					continue
				}
				return &teamSearchResult{displayName: name, teamID: teamID}, nil
			}
		}
	}

	return nil, fmt.Errorf("no issue found with team matching %q", query)
}

// extractTeamID extracts the team UUID from a Jira team field value.
// The value can be {"id": "uuid", ...} or a plain string UUID.
func extractTeamID(data json.RawMessage) string {
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &obj); err == nil && obj.ID != "" {
		return obj.ID
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil && s != "" {
		return s
	}
	return ""
}

// extractTeamName tries to extract a human-readable team name from a Jira field value.
// Team fields can have various structures depending on the Jira configuration.
func extractTeamName(data json.RawMessage) string {
	// Try: {"name": "..."} or {"displayName": "..."} or {"title": "..."}
	var obj struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Title       string `json:"title"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		if obj.Name != "" {
			return obj.Name
		}
		if obj.DisplayName != "" {
			return obj.DisplayName
		}
		if obj.Title != "" {
			return obj.Title
		}
	}

	// Try: plain string value.
	var s string
	if err := json.Unmarshal(data, &s); err == nil && s != "" {
		return s
	}

	return ""
}

// teamSearchResult holds a resolved team from one of the search strategies.
type teamSearchResult struct {
	displayName string
	teamID      string // the team UUID
}

// searchTeamsAPI tries multiple Jira/Atlassian team search endpoints.
func (c *Client) searchTeamsAPI(query string) (*teamSearchResult, error) {
	// Endpoints to try, in order of likelihood.
	endpoints := []string{
		fmt.Sprintf("%s/rest/teams/1.0/teams/find?query=%s", c.baseURL, url.QueryEscape(query)),
		fmt.Sprintf("%s/rest/teams/1.0/teams?query=%s", c.baseURL, url.QueryEscape(query)),
	}

	for _, ep := range endpoints {
		req, err := http.NewRequest(http.MethodGet, ep, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.authRequest(req); err != nil {
			continue
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			continue
		}

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}

		// Parse the response — it can be an array or an object with a "teams" key.
		teams := c.parseTeamsResponse(respBody)

		// Find best match by name.
		q := strings.ToLower(query)
		for _, t := range teams {
			dn := strings.ToLower(t.DisplayName)
			if dn == q || strings.Contains(dn, q) || strings.Contains(q, dn) {
				return &teamSearchResult{displayName: t.DisplayName, teamID: t.TeamID}, nil
			}
		}
	}

	return nil, fmt.Errorf("teams API search found no match for %q", query)
}

// jiraTeamEntry represents a team from the Jira Teams API response.
type jiraTeamEntry struct {
	TeamID      string `json:"teamId"`
	DisplayName string `json:"displayName"`
	// Some responses use different field names.
	ID    string `json:"id"`
	Name  string `json:"name"`
	Title string `json:"title"`
}

// effectiveID returns the team's ID, checking multiple possible field names.
func (t jiraTeamEntry) effectiveID() string {
	if t.TeamID != "" {
		return t.TeamID
	}
	return t.ID
}

// effectiveName returns the team's display name, checking multiple possible field names.
func (t jiraTeamEntry) effectiveName() string {
	if t.DisplayName != "" {
		return t.DisplayName
	}
	if t.Name != "" {
		return t.Name
	}
	return t.Title
}

// normalizedTeam returns a team entry with normalized fields.
type normalizedTeam struct {
	TeamID      string
	DisplayName string
}

// parseTeamsResponse parses teams from various response formats.
func (c *Client) parseTeamsResponse(data []byte) []normalizedTeam {
	var result []normalizedTeam

	// Try 1: direct array of team objects.
	var arr []jiraTeamEntry
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 {
		for _, t := range arr {
			if id := t.effectiveID(); id != "" {
				result = append(result, normalizedTeam{TeamID: id, DisplayName: t.effectiveName()})
			}
		}
		if len(result) > 0 {
			return result
		}
	}

	// Try 2: object with "teams" key.
	var wrapper struct {
		Teams []jiraTeamEntry `json:"teams"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Teams) > 0 {
		for _, t := range wrapper.Teams {
			if id := t.effectiveID(); id != "" {
				result = append(result, normalizedTeam{TeamID: id, DisplayName: t.effectiveName()})
			}
		}
		if len(result) > 0 {
			return result
		}
	}

	// Try 3: object with "values" key (paginated response).
	var paginated struct {
		Values []jiraTeamEntry `json:"values"`
	}
	if err := json.Unmarshal(data, &paginated); err == nil && len(paginated.Values) > 0 {
		for _, t := range paginated.Values {
			if id := t.effectiveID(); id != "" {
				result = append(result, normalizedTeam{TeamID: id, DisplayName: t.effectiveName()})
			}
		}
	}

	return result
}

// ResolveUserViaIssues is a fallback when /user/search returns no results (e.g. the
// service account lacks "Browse users and groups" permission). It searches the default
// project for issues with assignees and matches the display name against the query.
// This works because the /search endpoint returns the assignee's accountId even when
// the user search endpoints are restricted.
func (c *Client) ResolveUserViaIssues(displayName string) ([]JiraUser, error) {
	// Search for recently-updated issues with an assignee in the default project.
	jql := fmt.Sprintf("project = %s AND assignee is not EMPTY ORDER BY updated DESC", c.projectKey)
	searchURL := fmt.Sprintf("%s/rest/api/3/search/jql?jql=%s&maxResults=50&fields=assignee",
		c.baseURL, url.QueryEscape(jql))

	req, err := http.NewRequest(http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, fmt.Errorf("auth request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Issues []struct {
			Fields struct {
				Assignee *struct {
					AccountID   string `json:"accountId"`
					DisplayName string `json:"displayName"`
					Active      bool   `json:"active"`
				} `json:"assignee"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	// Deduplicate users and find matches.
	seen := make(map[string]bool)
	queryLower := strings.ToLower(strings.TrimSpace(displayName))
	queryParts := strings.Fields(queryLower)
	var matches []JiraUser
	for _, issue := range result.Issues {
		a := issue.Fields.Assignee
		if a == nil || seen[a.AccountID] {
			continue
		}
		seen[a.AccountID] = true
		nameLower := strings.ToLower(a.DisplayName)
		// Match if the display name contains the full query or all query parts.
		matched := strings.Contains(nameLower, queryLower)
		if !matched && len(queryParts) > 1 {
			matched = true
			for _, p := range queryParts {
				if !strings.Contains(nameLower, p) {
					matched = false
					break
				}
			}
		}
		if matched {
			matches = append(matches, JiraUser{
				AccountID:   a.AccountID,
				DisplayName: a.DisplayName,
				Active:      a.Active,
			})
		}
	}

	return matches, nil
}

// SearchIssuesJQL searches for issues using JQL and returns a formatted summary.
// It follows nextPageToken until maxResults issues are read (Jira caps each page at 100);
// extra names further fields, by id or name, returned in IssueSummary.Extra.
func (c *Client) SearchIssuesJQL(jql string, maxResults int, extra ...string) ([]IssueSummary, error) {
	if maxResults <= 0 {
		maxResults = 20
	}
	refs, err := c.resolveFields(extra)
	if err != nil {
		return nil, err
	}
	fields := strings.Join(append([]string{"summary,status,assignee,reporter,priority,issuetype,project,labels,created,updated,description"}, append(c.extraFieldIDs(), fieldIDs(refs)...)...), ",")

	var allIssues []IssueSummary
	token := ""
	for {
		q := url.Values{"jql": {jql}, "maxResults": {strconv.Itoa(min(maxResults-len(allIssues), jiraPageSize))}, "fields": {fields}}
		if token != "" {
			q.Set("nextPageToken", token)
		}
		searchURL := c.baseURL + "/rest/api/3/search/jql?" + q.Encode()

		req, err := http.NewRequest(http.MethodGet, searchURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.authRequest(req); err != nil {
			return nil, fmt.Errorf("auth request: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("send request: %w", err)
		}

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
		}

		var result struct {
			Issues []struct {
				Key    string          `json:"key"`
				Fields json.RawMessage `json:"fields"`
			} `json:"issues"`
			NextPageToken string `json:"nextPageToken"`
			IsLast        bool   `json:"isLast"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}

		for _, i := range result.Issues {
			var fields struct {
				Summary     string                        `json:"summary"`
				Status      struct{ Name string }         `json:"status"`
				Assignee    *struct{ DisplayName string } `json:"assignee"`
				Reporter    *struct{ DisplayName string } `json:"reporter"`
				Priority    *struct{ Name string }        `json:"priority"`
				IssueType   struct{ Name string }         `json:"issuetype"`
				Project     struct{ Key string }          `json:"project"`
				Labels      []string                      `json:"labels"`
				Created     string                        `json:"created"`
				Updated     string                        `json:"updated"`
				Description json.RawMessage               `json:"description"`
			}
			_ = json.Unmarshal(i.Fields, &fields)

			assignee, reporter := "", ""
			if fields.Assignee != nil {
				assignee = fields.Assignee.DisplayName
			}
			if fields.Reporter != nil {
				reporter = fields.Reporter.DisplayName
			}
			priority := ""
			if fields.Priority != nil {
				priority = fields.Priority.Name
			}
			desc := adfToPlainText(fields.Description)

			team, sprint := c.extractCustomFields(i.Fields)

			allIssues = append(allIssues, IssueSummary{
				Key:         i.Key,
				Summary:     fields.Summary,
				Status:      fields.Status.Name,
				Assignee:    assignee,
				Reporter:    reporter,
				Priority:    priority,
				IssueType:   fields.IssueType.Name,
				Project:     fields.Project.Key,
				Labels:      fields.Labels,
				Created:     fields.Created,
				Updated:     fields.Updated,
				Description: desc,
				Browse:      fmt.Sprintf("%s/browse/%s", c.siteURL, i.Key),
				Team:        team,
				Sprint:      sprint,
				Extra:       fieldValues(i.Fields, refs),
			})
		}

		if len(allIssues) >= maxResults || result.IsLast || result.NextPageToken == "" || len(result.Issues) == 0 {
			break
		}
		token = result.NextPageToken
	}

	// Trim to exact maxResults in case the last page overshoots.
	if len(allIssues) > maxResults {
		allIssues = allIssues[:maxResults]
	}

	return allIssues, nil
}

// AssignedIssues returns the unresolved issues assigned to the account behind
// the client's credentials, most recently updated first.
func (c *Client) AssignedIssues(maxResults int) ([]IssueSummary, error) {
	return c.SearchIssuesJQL("assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC", maxResults)
}

// GetIssue fetches a single Jira issue by key with full details; extra names further fields, by id or name.
func (c *Client) GetIssue(issueKey string, extra ...string) (*IssueSummary, error) {
	refs, err := c.resolveFields(extra)
	if err != nil {
		return nil, err
	}
	fieldList := strings.Join(append([]string{"summary,status,assignee,priority,issuetype,updated,description,labels,reporter"}, append(c.extraFieldIDs(), fieldIDs(refs)...)...), ",")
	reqURL := c.baseURL + "/rest/api/3/issue/" + url.PathEscape(issueKey) + "?" + url.Values{"fields": {fieldList}}.Encode()

	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, fmt.Errorf("auth request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var raw struct {
		Key    string          `json:"key"`
		Fields json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	var fields struct {
		Summary     string                        `json:"summary"`
		Status      struct{ Name string }         `json:"status"`
		Assignee    *struct{ DisplayName string } `json:"assignee"`
		Reporter    *struct{ DisplayName string } `json:"reporter"`
		Priority    *struct{ Name string }        `json:"priority"`
		IssueType   struct{ Name string }         `json:"issuetype"`
		Updated     string                        `json:"updated"`
		Labels      []string                      `json:"labels"`
		Description json.RawMessage               `json:"description"`
	}
	_ = json.Unmarshal(raw.Fields, &fields)

	assignee := ""
	if fields.Assignee != nil {
		assignee = fields.Assignee.DisplayName
	}
	reporter := ""
	if fields.Reporter != nil {
		reporter = fields.Reporter.DisplayName
	}
	priority := ""
	if fields.Priority != nil {
		priority = fields.Priority.Name
	}
	desc := adfToPlainText(fields.Description)

	// Extract custom fields (Team, Sprint) from raw JSON.
	team, sprint := c.extractCustomFields(raw.Fields)

	return &IssueSummary{
		Key:         raw.Key,
		Summary:     fields.Summary,
		Status:      fields.Status.Name,
		Assignee:    assignee,
		Reporter:    reporter,
		Priority:    priority,
		IssueType:   fields.IssueType.Name,
		Updated:     fields.Updated,
		Labels:      fields.Labels,
		Description: desc,
		Browse:      fmt.Sprintf("%s/browse/%s", c.siteURL, raw.Key),
		Team:        team,
		Sprint:      sprint,
		Extra:       fieldValues(raw.Fields, refs),
	}, nil
}

// LabelChange is a single add/remove of one label, attributed to the Jira user
// who made the change, as recorded in the issue changelog.
type LabelChange struct {
	Action    string `json:"action"` // "added" or "removed"
	Label     string `json:"label"`
	Author    string `json:"author"`     // display name
	AccountID string `json:"account_id"` // Jira account ID
	Email     string `json:"email,omitempty"`
	Created   string `json:"created"` // raw Jira timestamp
}

// GetLabelChangeHistory reads the full changelog of an issue and returns every
// add/remove of the given label, in chronological order (oldest first). It is
// the authoritative way to determine WHO added a label — the issue fields alone
// (reporter/assignee) do not reveal label authorship. If label is empty, all
// label changes are returned. The changelog endpoint is paginated; this walks
// every page.
func (c *Client) GetLabelChangeHistory(issueKey, label string) ([]LabelChange, error) {
	const pageSize = 100
	target := strings.TrimSpace(label)

	var changes []LabelChange
	startAt := 0
	for page := 0; page < 100; page++ { // hard cap to avoid unbounded looping
		reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s/changelog?startAt=%d&maxResults=%d",
			c.baseURL, url.PathEscape(issueKey), startAt, pageSize)

		req, err := http.NewRequest(http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.authRequest(req); err != nil {
			return nil, fmt.Errorf("auth request: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("send request: %w", err)
		}
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
		}

		var pageResp struct {
			MaxResults int  `json:"maxResults"`
			Total      int  `json:"total"`
			IsLast     bool `json:"isLast"`
			Values     []struct {
				Created string `json:"created"`
				Author  struct {
					DisplayName  string `json:"displayName"`
					AccountID    string `json:"accountId"`
					EmailAddress string `json:"emailAddress"`
				} `json:"author"`
				Items []struct {
					Field      string `json:"field"`
					FieldID    string `json:"fieldId"`
					FromString string `json:"fromString"`
					ToString   string `json:"toString"`
				} `json:"items"`
			} `json:"values"`
		}
		if err := json.Unmarshal(respBody, &pageResp); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}

		for _, h := range pageResp.Values {
			for _, it := range h.Items {
				if it.Field != "labels" && it.FieldID != "labels" {
					continue
				}
				added, removed := diffLabelTokens(it.FromString, it.ToString)
				record := func(action string, labels []string) {
					for _, lbl := range labels {
						if target != "" && lbl != target {
							continue
						}
						changes = append(changes, LabelChange{
							Action:    action,
							Label:     lbl,
							Author:    h.Author.DisplayName,
							AccountID: h.Author.AccountID,
							Email:     h.Author.EmailAddress,
							Created:   h.Created,
						})
					}
				}
				record("added", added)
				record("removed", removed)
			}
		}

		startAt += pageSize
		if pageResp.IsLast || len(pageResp.Values) == 0 ||
			(pageResp.Total > 0 && startAt >= pageResp.Total) {
			break
		}
	}

	return changes, nil
}

// diffLabelTokens returns the labels added (in to but not from) and removed
// (in from but not to) between two space-separated Jira label strings.
func diffLabelTokens(from, to string) (added, removed []string) {
	fromSet := map[string]bool{}
	for _, t := range strings.Fields(from) {
		fromSet[t] = true
	}
	toSet := map[string]bool{}
	for _, t := range strings.Fields(to) {
		toSet[t] = true
	}
	for t := range toSet {
		if !fromSet[t] {
			added = append(added, t)
		}
	}
	for t := range fromSet {
		if !toSet[t] {
			removed = append(removed, t)
		}
	}
	return added, removed
}

// UpdateIssueDescription updates only the description of a Jira issue using ADF format.
func (c *Client) UpdateIssueDescription(issueKey, description string) error {
	adf := textToADF(description)
	payload := map[string]interface{}{
		"fields": map[string]interface{}{
			"description": adf,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s", c.baseURL, issueKey)
	req, err := http.NewRequest(http.MethodPut, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authRequest(req); err != nil {
		return fmt.Errorf("auth request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// IssueSummary represents a Jira issue with common fields.
type IssueSummary struct {
	Key         string       `json:"key"`
	Summary     string       `json:"summary"`
	Status      string       `json:"status"`
	Assignee    string       `json:"assignee,omitempty"`
	Reporter    string       `json:"reporter,omitempty"`
	Priority    string       `json:"priority,omitempty"`
	IssueType   string       `json:"issue_type"`
	Project     string       `json:"project,omitempty"`
	Created     string       `json:"created,omitempty"`
	Updated     string       `json:"updated"`
	Labels      []string     `json:"labels,omitempty"`
	Description string       `json:"description,omitempty"`
	Browse      string       `json:"browse"`
	Team        string       `json:"team,omitempty"`
	Sprint      string       `json:"sprint,omitempty"`
	Extra       []FieldValue `json:"extra,omitempty"`
}

// FieldValue is one requested issue field as text; Missing says why Jira gave no value, otherwise an empty Text means the field is empty.
type FieldValue struct {
	Name    string `json:"name"`
	ID      string `json:"id,omitempty"`
	Text    string `json:"text,omitempty"`
	Missing string `json:"missing,omitempty"`
}

const (
	maxExtraFields    = 10
	maxFieldRef       = 100
	fieldCatalogTTL   = time.Hour
	fieldCatalogRetry = time.Minute
)

var fieldIDPattern = regexp.MustCompile(`^[a-z][A-Za-z0-9_.-]{0,63}$`)

type fieldCatalog struct {
	ids    map[string]string   // lowercased id to id
	byName map[string][]string // lowercased name or JQL clause name to ids
}

func (c *Client) fieldCatalog() *fieldCatalog {
	c.fieldsMu.Lock()
	defer c.fieldsMu.Unlock()
	now := time.Now()
	if c.catalog != nil && now.Sub(c.catalogAt) < fieldCatalogTTL || now.Sub(c.catalogTried) < fieldCatalogRetry {
		return c.catalog
	}
	c.catalogTried = now
	list, err := c.fetchFields()
	if err != nil {
		c.catalogTried = time.Now()
		return c.catalog
	}
	cat := &fieldCatalog{ids: make(map[string]string, len(list)), byName: map[string][]string{}}
	for _, f := range list {
		cat.ids[strings.ToLower(f.ID)] = f.ID
		for _, n := range append([]string{f.Name}, f.ClauseNames...) {
			k := strings.ToLower(strings.TrimSpace(n))
			if k != "" && !slices.Contains(cat.byName[k], f.ID) {
				cat.byName[k] = append(cat.byName[k], f.ID)
			}
		}
	}
	c.catalog, c.catalogAt = cat, now
	return cat
}

type fieldRef struct {
	asked   string
	id      string
	problem string
}

// resolveFields maps the requested field ids or names to the ids Jira returns.
func (c *Client) resolveFields(extra []string) ([]fieldRef, error) {
	var refs []fieldRef
	seen := map[string]bool{}
	for _, f := range extra {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		if len(f) > maxFieldRef || strings.ContainsFunc(f, unicode.IsControl) {
			return nil, fmt.Errorf("invalid field %q", f)
		}
		seen[f] = true
		refs = append(refs, fieldRef{asked: f})
	}
	if len(refs) > maxExtraFields {
		return nil, fmt.Errorf("at most %d extra fields per request", maxExtraFields)
	}
	if len(refs) == 0 {
		return nil, nil
	}
	cat := c.fieldCatalog()
	for i := range refs {
		r := &refs[i]
		key := strings.ToLower(r.asked)
		switch {
		case cat != nil && cat.ids[key] == r.asked:
			r.id = r.asked
		case cat != nil && len(cat.byName[key]) == 1:
			r.id = cat.byName[key][0]
		case cat != nil && len(cat.byName[key]) > 1:
			r.problem = "several fields have this name (" + strings.Join(cat.byName[key], ", ") + "); ask for one by id"
		case cat != nil && cat.ids[key] != "":
			r.id = cat.ids[key]
		case fieldIDPattern.MatchString(r.asked):
			// Not in the catalog, which may predate the field: Jira decides.
			r.id = r.asked
		case cat == nil:
			r.problem = "field names cannot be looked up right now; ask by field id"
		default:
			r.problem = "no field with this id or name is visible to arbetern"
		}
	}
	return refs, nil
}

func fieldIDs(refs []fieldRef) []string {
	var ids []string
	for _, r := range refs {
		if r.id != "" && !slices.Contains(ids, r.id) {
			ids = append(ids, r.id)
		}
	}
	return ids
}

func fieldValues(raw json.RawMessage, refs []fieldRef) []FieldValue {
	if len(refs) == 0 {
		return nil
	}
	var all map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&all)
	out := make([]FieldValue, 0, len(refs))
	for _, r := range refs {
		fv := FieldValue{Name: r.asked, ID: r.id, Missing: r.problem}
		if fv.Missing == "" {
			if v, ok := all[r.id]; ok {
				fv.Text = fieldText(v)
			} else {
				fv.Missing = "Jira returned no such field for this issue"
			}
		}
		out = append(out, fv)
	}
	return out
}

func fieldText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return fmt.Sprint(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := fieldText(e); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return jsonText(parts)
	case map[string]any:
		if t["type"] == "doc" {
			raw, _ := json.Marshal(t)
			return adfToPlainText(raw)
		}
		if link := issueLinkText(t); link != "" {
			return link
		}
		for _, k := range []string{"value", "displayName", "name", "filename", "key"} {
			s, ok := t[k].(string)
			if !ok || s == "" {
				continue
			}
			if child := fieldText(t["child"]); child != "" {
				return s + " / " + child
			}
			return s
		}
		rest := make(map[string]any, len(t))
		for k, v := range t {
			switch k {
			case "self", "avatarUrls", "iconUrl", "_links", "expand":
			default:
				rest[k] = v
			}
		}
		return jsonText(rest)
	}
	return ""
}

func issueLinkText(m map[string]any) string {
	typ, _ := m["type"].(map[string]any)
	for _, side := range [][2]string{{"outwardIssue", "outward"}, {"inwardIssue", "inward"}} {
		if issue, ok := m[side[0]].(map[string]any); ok {
			verb, _ := typ[side[1]].(string)
			key, _ := issue["key"].(string)
			return strings.TrimSpace(verb + " " + key)
		}
	}
	return ""
}

func jsonText(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSpace(b.String())
}

// adfToPlainText extracts plain text from an ADF document (json.RawMessage).
func adfToPlainText(data json.RawMessage) string {
	if len(data) == 0 || string(data) == "null" {
		return ""
	}
	var doc struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, node := range doc.Content {
		extractText(node, &sb)
	}
	return strings.TrimSpace(sb.String())
}

// extractText recursively extracts text from ADF nodes.
func extractText(data json.RawMessage, sb *strings.Builder) {
	var node struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Attrs struct {
			Text      string `json:"text"`
			URL       string `json:"url"`
			ShortName string `json:"shortName"`
			Timestamp string `json:"timestamp"`
		} `json:"attrs"`
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &node); err != nil {
		return
	}
	switch {
	case node.Type == "mention" && node.Attrs.Text != "":
		// Render mentions as their @-prefixed display name so downstream
		// LLM consumers see the same thing a human would in the UI.
		sb.WriteString(node.Attrs.Text)
	case node.Type == "hardBreak":
		sb.WriteString("\n")
	case node.Type == "inlineCard" || node.Type == "blockCard" || node.Type == "embedCard":
		sb.WriteString(node.Attrs.URL)
	case node.Type == "status" || node.Type == "emoji":
		sb.WriteString(cmp.Or(node.Attrs.Text, node.Attrs.ShortName))
	case node.Type == "date":
		sb.WriteString(adfDate(node.Attrs.Timestamp))
	case node.Text != "":
		sb.WriteString(node.Text)
	}
	for _, child := range node.Content {
		extractText(child, sb)
	}
	if node.Type == "paragraph" || node.Type == "heading" || node.Type == "bulletList" || node.Type == "orderedList" || node.Type == "listItem" || node.Type == "blockCard" || node.Type == "embedCard" {
		sb.WriteString("\n")
	}
}

// adfDate renders an ADF date node's timestamp, documented as milliseconds; smaller values are read as seconds.
func adfDate(ts string) string {
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ts
	}
	if n > 0 && n < 1e11 {
		n *= 1000
	}
	return time.UnixMilli(n).UTC().Format(time.DateOnly)
}

// UpdateIssueFields sets arbitrary fields on an existing Jira issue.
func (c *Client) UpdateIssueFields(issueKey string, fields map[string]interface{}) error {
	payload := map[string]interface{}{
		"fields": fields,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s", c.baseURL, issueKey)
	req, err := http.NewRequest(http.MethodPut, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authRequest(req); err != nil {
		return fmt.Errorf("auth request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// Dashboards & Filters

// Dashboard represents a Jira dashboard.
type Dashboard struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Owner       struct {
		DisplayName string `json:"displayName"`
		AccountID   string `json:"accountId"`
	} `json:"owner"`
	View    string `json:"view"` // absolute URL to the dashboard in the UI
	Ranking int    `json:"ranking,omitempty"`
}

// DashboardGadget represents a gadget on a Jira dashboard.
type DashboardGadget struct {
	ID        json.Number `json:"id"`
	Title     string      `json:"title"`
	ModuleKey string      `json:"moduleKey,omitempty"`
	URI       string      `json:"uri,omitempty"`
	Color     string      `json:"color,omitempty"`
	Position  struct {
		Row    int `json:"row"`
		Column int `json:"column"`
	} `json:"position"`
}

// FilterDetail represents a Jira saved filter.
type FilterDetail struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	JQL         string `json:"jql"`
	Owner       struct {
		DisplayName string `json:"displayName"`
		AccountID   string `json:"accountId"`
	} `json:"owner"`
	ViewURL    string `json:"viewUrl"`
	Favourite  bool   `json:"favourite"`
	SharePerms []struct {
		Type string `json:"type"` // "global", "project", "group", etc.
	} `json:"sharePermissions"`
}

// GetDashboard fetches a Jira dashboard by its numeric ID.
func (c *Client) GetDashboard(dashboardID string) (*Dashboard, error) {
	endpoint := fmt.Sprintf("%s/rest/api/3/dashboard/%s", c.baseURL, url.PathEscape(dashboardID))

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dashboard request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read dashboard response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dashboard returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var dash Dashboard
	if err := json.Unmarshal(body, &dash); err != nil {
		return nil, fmt.Errorf("unmarshal dashboard: %w", err)
	}
	return &dash, nil
}

// GetDashboardGadgets fetches all gadgets configured on a Jira dashboard.
func (c *Client) GetDashboardGadgets(dashboardID string) ([]DashboardGadget, error) {
	endpoint := fmt.Sprintf("%s/rest/api/3/dashboard/%s/gadget", c.baseURL, url.PathEscape(dashboardID))

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dashboard gadgets request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read gadgets response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dashboard gadgets returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Gadgets []DashboardGadget `json:"gadgets"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal gadgets: %w", err)
	}
	return result.Gadgets, nil
}

// GetFilter fetches a Jira saved filter by its numeric ID.
func (c *Client) GetFilter(filterID string) (*FilterDetail, error) {
	endpoint := fmt.Sprintf("%s/rest/api/3/filter/%s", c.baseURL, url.PathEscape(filterID))

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("filter request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read filter response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("filter returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var filter FilterDetail
	if err := json.Unmarshal(body, &filter); err != nil {
		return nil, fmt.Errorf("unmarshal filter: %w", err)
	}
	return &filter, nil
}

// Comments, issue links, sprints

// marshalCreateIssueBody serializes the typed createIssuePayload while merging
// arbitrary extra fields supplied via CreateIssueInput.Fields. Typed fields
// already populated on the payload (Project, Summary, IssueType, Description,
// Labels, Assignee) take precedence — extras never overwrite them.
func marshalCreateIssueBody(payload createIssuePayload, extras map[string]interface{}) ([]byte, error) {
	if len(extras) == 0 {
		return json.Marshal(payload)
	}

	// Marshal the typed payload then re-decode into a generic map so we can
	// overlay extras without losing the strict typing/omitempty semantics.
	typedBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Fields map[string]interface{} `json:"fields"`
	}
	if err := json.Unmarshal(typedBytes, &wrapper); err != nil {
		return nil, err
	}
	if wrapper.Fields == nil {
		wrapper.Fields = map[string]interface{}{}
	}
	for k, v := range extras {
		if _, taken := wrapper.Fields[k]; taken {
			// Typed payload already set this key — never let extras
			// silently clobber a typed value.
			continue
		}
		wrapper.Fields[k] = v
	}
	return json.Marshal(wrapper)
}

// Comment represents a Jira issue comment.
type Comment struct {
	ID     string `json:"id"`
	Author struct {
		AccountID   string `json:"accountId"`
		DisplayName string `json:"displayName"`
		EmailAddr   string `json:"emailAddress,omitempty"`
	} `json:"author"`
	Body    string `json:"-"` // plain-text rendering of the ADF body
	Created string `json:"created"`
	Updated string `json:"updated"`
}

// commentEnvelope mirrors the Jira REST API's comment shape, where the body is
// an ADF document on writes/reads. We render it down to plain text for
// downstream LLM consumption.
type commentEnvelope struct {
	ID     string `json:"id"`
	Author struct {
		AccountID   string `json:"accountId"`
		DisplayName string `json:"displayName"`
		EmailAddr   string `json:"emailAddress,omitempty"`
	} `json:"author"`
	Body    json.RawMessage `json:"body"`
	Created string          `json:"created"`
	Updated string          `json:"updated"`
}

func (e commentEnvelope) toComment() Comment {
	return Comment{
		ID:      e.ID,
		Author:  e.Author,
		Body:    adfToPlainText(e.Body),
		Created: e.Created,
		Updated: e.Updated,
	}
}

// AddComment posts a comment on a Jira issue. The body is converted from
// markdown-flavoured text to ADF using the same converter used by issue
// descriptions. Returns the created comment.
// IssueTransition is a workflow step an issue can take now; Category is its target status category: new, indeterminate or done.
type IssueTransition struct {
	ID       string
	Name     string
	To       string
	Category string
}

// IssueTransitions returns the category of an issue's current status and the transitions it can take now.
func (c *Client) IssueTransitions(issueKey string) (string, []IssueTransition, error) {
	var issue struct {
		Fields struct {
			Status struct {
				StatusCategory struct {
					Key string `json:"key"`
				} `json:"statusCategory"`
			} `json:"status"`
		} `json:"fields"`
	}
	if err := c.jiraJSON(http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(issueKey)+"?fields=status", nil, &issue); err != nil {
		return "", nil, err
	}
	var list struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name           string `json:"name"`
				StatusCategory struct {
					Key string `json:"key"`
				} `json:"statusCategory"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := c.jiraJSON(http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(issueKey)+"/transitions", nil, &list); err != nil {
		return "", nil, err
	}
	out := make([]IssueTransition, 0, len(list.Transitions))
	for _, t := range list.Transitions {
		out = append(out, IssueTransition{ID: t.ID, Name: t.Name, To: t.To.Name, Category: t.To.StatusCategory.Key})
	}
	return issue.Fields.Status.StatusCategory.Key, out, nil
}

// TransitionIssue moves an issue through the transition with the given id.
func (c *Client) TransitionIssue(issueKey, transitionID string) error {
	body := map[string]any{"transition": map[string]string{"id": transitionID}}
	return c.jiraJSON(http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(issueKey)+"/transitions", body, nil)
}

func (c *Client) jiraJSON(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal payload: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.authRequest(req); err != nil {
		return fmt.Errorf("auth request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(data))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) AddComment(issueKey, body string) (*Comment, error) {
	if strings.TrimSpace(issueKey) == "" {
		return nil, fmt.Errorf("issue key is required")
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("comment body is required")
	}
	doc := textToADF(body)
	if doc == nil {
		return nil, fmt.Errorf("comment body produced empty ADF document")
	}
	payload := map[string]interface{}{"body": doc}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s/comment", c.baseURL, url.PathEscape(issueKey))
	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, fmt.Errorf("auth request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}
	var env commentEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	cm := env.toComment()
	return &cm, nil
}

// ListComments returns the most recent comments on a Jira issue, newest first.
// limit caps the number returned (server-side ordering is ascending by
// created date, so we fetch up to limit*2 and reverse client-side). Pass
// limit <= 0 to use a default of 20. limit is capped at 100.
func (c *Client) ListComments(issueKey string, limit int) ([]Comment, error) {
	if strings.TrimSpace(issueKey) == "" {
		return nil, fmt.Errorf("issue key is required")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	// orderBy=-created asks Jira for newest first; older Jira deployments
	// ignore the param and return ascending order, so we re-sort below.
	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s/comment?orderBy=-created&maxResults=%d",
		c.baseURL, url.PathEscape(issueKey), limit)
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, fmt.Errorf("auth request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}
	var result struct {
		Comments []commentEnvelope `json:"comments"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	out := make([]Comment, 0, len(result.Comments))
	for _, env := range result.Comments {
		out = append(out, env.toComment())
	}
	// Force newest-first regardless of server's ordering behaviour. Created
	// is an ISO-8601 string so lexicographic comparison sorts correctly.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		if out[i].Created < out[j].Created {
			out[i], out[j] = out[j], out[i]
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LinkIssues creates a directional link between two issues. linkType is the
// relationship name as it appears in Jira (e.g. "Relates", "Blocks",
// "Causes", "Duplicates"). The inward issue is the "source" and the outward
// issue is the "target": for "Blocks", inward blocks outward.
func (c *Client) LinkIssues(inwardKey, outwardKey, linkType string) error {
	inwardKey = strings.TrimSpace(inwardKey)
	outwardKey = strings.TrimSpace(outwardKey)
	linkType = strings.TrimSpace(linkType)
	if inwardKey == "" || outwardKey == "" {
		return fmt.Errorf("both inward and outward issue keys are required")
	}
	if linkType == "" {
		return fmt.Errorf("link type is required (e.g. Relates, Blocks, Causes, Duplicates)")
	}
	payload := map[string]interface{}{
		"type":         map[string]string{"name": linkType},
		"inwardIssue":  map[string]string{"key": inwardKey},
		"outwardIssue": map[string]string{"key": outwardKey},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	reqURL := fmt.Sprintf("%s/rest/api/3/issueLink", c.baseURL)
	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authRequest(req); err != nil {
		return fmt.Errorf("auth request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return fmt.Errorf("jira API error (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// Sprint represents a Jira Software (Agile) sprint.
type Sprint struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	State        string `json:"state"` // "active", "closed", "future"
	BoardID      int    `json:"originBoardId"`
	StartDate    string `json:"startDate,omitempty"`
	EndDate      string `json:"endDate,omitempty"`
	CompleteDate string `json:"completeDate,omitempty"`
	Goal         string `json:"goal,omitempty"`
}

// SprintFieldID returns the customfield_XXXXX id of the Sprint field. Triggers
// lazy discovery on first call. Returns "" if the field could not be located
// (e.g. instance has no Jira Software / scrum boards).
func (c *Client) SprintFieldID() string {
	_, sprint := c.discoverExtraFields()
	return sprint
}

// GetActiveSprintForProject locates the scrum board associated with a project
// and returns its currently active sprint, if any. projectKey defaults to the
// client's configured default project. Returns (nil, nil) when no scrum board
// or active sprint exists — that's a normal state, not an error.
func (c *Client) GetActiveSprintForProject(projectKey string) (*Sprint, error) {
	if strings.TrimSpace(projectKey) == "" {
		projectKey = c.projectKey
	}
	if projectKey == "" {
		return nil, fmt.Errorf("project key is required (no default configured)")
	}

	// Find scrum boards for the project. Kanban boards don't have sprints.
	boardURL := fmt.Sprintf("%s/rest/agile/1.0/board?projectKeyOrId=%s&type=scrum",
		c.baseURL, url.QueryEscape(projectKey))
	req, err := http.NewRequest(http.MethodGet, boardURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create board request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, fmt.Errorf("auth board request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("board request failed: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("board lookup HTTP %d: %s", resp.StatusCode, string(body))
	}
	var boards struct {
		Values []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"values"`
	}
	if err := json.Unmarshal(body, &boards); err != nil {
		return nil, fmt.Errorf("unmarshal boards: %w", err)
	}
	if len(boards.Values) == 0 {
		return nil, nil
	}

	// Walk scrum boards in order; first active sprint wins. Multiple boards
	// per project is rare but real (e.g. team-specific scrum boards under
	// one project), and we don't have a signal to pick a "best" one.
	// A board that cannot be read is skipped, but when no board yields a sprint its error is returned, not "no sprint".
	var failed error
	for _, b := range boards.Values {
		sprintURL := fmt.Sprintf("%s/rest/agile/1.0/board/%d/sprint?state=active",
			c.baseURL, b.ID)
		sreq, err := http.NewRequest(http.MethodGet, sprintURL, nil)
		if err != nil {
			failed = cmp.Or(failed, err)
			continue
		}
		sreq.Header.Set("Accept", "application/json")
		if err := c.authRequest(sreq); err != nil {
			failed = cmp.Or(failed, err)
			continue
		}
		sresp, err := c.httpClient.Do(sreq)
		if err != nil {
			failed = cmp.Or(failed, err)
			continue
		}
		sbody, _ := io.ReadAll(io.LimitReader(sresp.Body, maxResponseBody))
		_ = sresp.Body.Close()
		if sresp.StatusCode < 200 || sresp.StatusCode >= 300 {
			failed = cmp.Or(failed, fmt.Errorf("board %d sprint lookup HTTP %d", b.ID, sresp.StatusCode))
			continue
		}
		var sprints struct {
			Values []Sprint `json:"values"`
		}
		if err := json.Unmarshal(sbody, &sprints); err != nil {
			failed = cmp.Or(failed, fmt.Errorf("board %d sprints: %w", b.ID, err))
			continue
		}
		if len(sprints.Values) > 0 {
			s := sprints.Values[0]
			if s.BoardID == 0 {
				s.BoardID = b.ID
			}
			return &s, nil
		}
	}
	return nil, failed
}

// SetSprintField assigns an issue to a sprint by ID. Jira Cloud's create-issue
// and update-issue endpoints reject the array form Jira returns on reads —
// the write contract is a SCALAR sprint id (not [id]) on the Sprint custom
// field. Discovery of the field id is automatic; callers can pre-flight with
// SprintFieldID() to bail out gracefully when no Sprint field exists.
func (c *Client) SetSprintField(issueKey string, sprintID int) error {
	if strings.TrimSpace(issueKey) == "" {
		return fmt.Errorf("issue key is required")
	}
	if sprintID <= 0 {
		return fmt.Errorf("sprintID must be > 0")
	}
	fid := c.SprintFieldID()
	if fid == "" {
		return fmt.Errorf("sprint custom field not found on this Jira instance")
	}
	return c.UpdateIssueFields(issueKey, map[string]interface{}{
		fid: sprintID,
	})
}
