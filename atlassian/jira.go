package atlassian

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Jira REST API v3
// ---------------------------------------------------------------------------

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

// parseInlineMarkdown converts simple inline markdown (**bold**, `code`, [text](url)) to ADF inlines.
func parseInlineMarkdown(text string) []adfInline {
	var inlines []adfInline
	remaining := text
	for len(remaining) > 0 {
		// Find the next special marker.
		boldIdx := strings.Index(remaining, "**")
		codeIdx := strings.Index(remaining, "`")
		linkIdx := strings.Index(remaining, "[")

		// No more markers.
		if boldIdx < 0 && codeIdx < 0 && linkIdx < 0 {
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
		if boldIdx >= 0 {
			candidates = append(candidates, candidate{boldIdx, "**"})
		}
		if codeIdx >= 0 {
			candidates = append(candidates, candidate{codeIdx, "`"})
		}
		if linkIdx >= 0 {
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

	body, err := json.Marshal(payload)
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
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Active      bool   `json:"active"`
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

// discoverExtraFields lazily discovers custom field IDs for Team and Sprint.
// Called once; results are cached on the Client.
func (c *Client) discoverExtraFields() {
	c.extraFieldsOnce.Do(func() {
		reqURL := fmt.Sprintf("%s/rest/api/3/field", c.baseURL)
		req, err := http.NewRequest(http.MethodGet, reqURL, nil)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.authRequest(req); err != nil {
			return
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return
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
		if err := json.Unmarshal(body, &fields); err != nil {
			return
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
	})
}

// extraFieldIDs returns the custom field IDs to append to search queries.
func (c *Client) extraFieldIDs() []string {
	c.discoverExtraFields()
	var ids []string
	if c.teamFieldID != "" {
		ids = append(ids, c.teamFieldID)
	}
	if c.sprintFieldID != "" {
		ids = append(ids, c.sprintFieldID)
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
	if c.teamFieldID != "" {
		if v, ok := allFields[c.teamFieldID]; ok {
			team = extractTeamName(v)
		}
	}
	if c.sprintFieldID != "" {
		if v, ok := allFields[c.sprintFieldID]; ok {
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
// Paginates through results using startAt/maxResults until all issues up to
// maxResults are fetched (Jira caps each page at 100).
func (c *Client) SearchIssuesJQL(jql string, maxResults int) ([]IssueSummary, error) {
	if maxResults <= 0 {
		maxResults = 20
	}
	fields := "summary,status,assignee,priority,issuetype,updated,description"
	for _, id := range c.extraFieldIDs() {
		fields += "," + id
	}

	var allIssues []IssueSummary
	startAt := 0
	pageSize := maxResults
	if pageSize > jiraPageSize {
		pageSize = jiraPageSize
	}

	for {
		searchURL := fmt.Sprintf("%s/rest/api/3/search/jql?jql=%s&startAt=%d&maxResults=%d&fields=%s",
			c.baseURL, url.QueryEscape(jql), startAt, pageSize, fields)

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
			StartAt int `json:"startAt"`
			Total   int `json:"total"`
			Issues  []struct {
				Key    string          `json:"key"`
				Fields json.RawMessage `json:"fields"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}

		for _, i := range result.Issues {
			var fields struct {
				Summary     string                        `json:"summary"`
				Status      struct{ Name string }         `json:"status"`
				Assignee    *struct{ DisplayName string } `json:"assignee"`
				Priority    *struct{ Name string }        `json:"priority"`
				IssueType   struct{ Name string }         `json:"issuetype"`
				Updated     string                        `json:"updated"`
				Description json.RawMessage               `json:"description"`
			}
			_ = json.Unmarshal(i.Fields, &fields)

			assignee := ""
			if fields.Assignee != nil {
				assignee = fields.Assignee.DisplayName
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
				Priority:    priority,
				IssueType:   fields.IssueType.Name,
				Updated:     fields.Updated,
				Description: desc,
				Browse:      fmt.Sprintf("%s/browse/%s", c.siteURL, i.Key),
				Team:        team,
				Sprint:      sprint,
			})
		}

		// Stop if we've reached the caller's desired count or exhausted results.
		if len(allIssues) >= maxResults || startAt+len(result.Issues) >= result.Total || len(result.Issues) == 0 {
			break
		}
		startAt += len(result.Issues)
		// Shrink page size for the final page if needed.
		remaining := maxResults - len(allIssues)
		if remaining < pageSize {
			pageSize = remaining
		}
	}

	// Trim to exact maxResults in case the last page overshoots.
	if len(allIssues) > maxResults {
		allIssues = allIssues[:maxResults]
	}

	return allIssues, nil
}

// GetIssue fetches a single Jira issue by key with full details.
func (c *Client) GetIssue(issueKey string) (*IssueSummary, error) {
	fieldList := "summary,status,assignee,priority,issuetype,updated,description,labels,reporter"
	for _, id := range c.extraFieldIDs() {
		fieldList += "," + id
	}
	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=%s",
		c.baseURL, issueKey, fieldList)

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
	}, nil
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
	Key         string   `json:"key"`
	Summary     string   `json:"summary"`
	Status      string   `json:"status"`
	Assignee    string   `json:"assignee,omitempty"`
	Reporter    string   `json:"reporter,omitempty"`
	Priority    string   `json:"priority,omitempty"`
	IssueType   string   `json:"issue_type"`
	Updated     string   `json:"updated"`
	Labels      []string `json:"labels,omitempty"`
	Description string   `json:"description,omitempty"`
	Browse      string   `json:"browse"`
	Team        string   `json:"team,omitempty"`
	Sprint      string   `json:"sprint,omitempty"`
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
		Type    string            `json:"type"`
		Text    string            `json:"text"`
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &node); err != nil {
		return
	}
	if node.Text != "" {
		sb.WriteString(node.Text)
	}
	for _, child := range node.Content {
		extractText(child, sb)
	}
	if node.Type == "paragraph" || node.Type == "heading" || node.Type == "bulletList" || node.Type == "orderedList" || node.Type == "listItem" {
		sb.WriteString("\n")
	}
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

// ---------------------------------------------------------------------------
// Dashboards & Filters
// ---------------------------------------------------------------------------

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
