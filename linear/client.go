package linear

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	linearAPIURL      = "https://api.linear.app/graphql"
	maxResponseBody   = 10 << 20 // 10 MB
	defaultMaxResults = 20
	maxAllowedResults = 50
	maxRetries        = 3
	retryBaseDelay    = 1 * time.Second
)

// Client provides access to the Linear GraphQL API.
type Client struct {
	apiToken   string
	teamID     string
	httpClient *http.Client
}

// NewClient creates a Linear API client using a personal API token.
func NewClient(apiToken, teamID string) *Client {
	// Linear expects a bare token, not "Bearer <token>".
	apiToken = strings.TrimPrefix(apiToken, "Bearer ")
	return &Client{
		apiToken:   apiToken,
		teamID:     teamID,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// graphQLRequest sends a GraphQL request and decodes the response into v.
// Retries on 429 (rate limit) and 5xx responses with exponential backoff.
func (c *Client) graphQLRequest(query string, variables map[string]interface{}, v interface{}) error {
	payload := map[string]interface{}{
		"query": query,
	}
	if len(variables) > 0 {
		payload["variables"] = variables
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	var respBody []byte
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequest(http.MethodPost, linearAPIURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Authorization", c.apiToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("send request: %w", err)
		}

		respBody, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}

		if resp.StatusCode == http.StatusOK {
			break
		}

		// Retry on 429 (rate limit) and 5xx (server errors).
		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < maxRetries {
			delay := retryBaseDelay * time.Duration(1<<uint(attempt))
			log.Printf("[linear] HTTP %d, retrying in %s (attempt %d/%d)", resp.StatusCode, delay, attempt+1, maxRetries)
			time.Sleep(delay)
			continue
		}

		return fmt.Errorf("linear API returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	if len(result.Errors) > 0 {
		msgs := make([]string, len(result.Errors))
		for i, e := range result.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("linear GraphQL errors: %s", strings.Join(msgs, "; "))
	}

	if err := json.Unmarshal(result.Data, v); err != nil {
		return fmt.Errorf("unmarshal data: %w", err)
	}
	return nil
}

// Issue represents a Linear issue.
type Issue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"` // e.g. "ENG-123"
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	State       State  `json:"state"`
	Assignee    *User  `json:"assignee"`
	Priority    int    `json:"priority"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// PriorityLabel returns a human-readable label for the Linear priority integer.
func (i *Issue) PriorityLabel() string {
	switch i.Priority {
	case 1:
		return "Urgent"
	case 2:
		return "High"
	case 3:
		return "Medium"
	case 4:
		return "Low"
	default:
		return "No priority"
	}
}

// State represents a Linear workflow state.
type State struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// User represents a Linear team member.
type User struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

// Team represents a Linear team.
type Team struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Key        string `json:"key"`
	IssueCount int    `json:"issueCount"`
}

// CreateIssueInput holds parameters for creating a Linear issue.
type CreateIssueInput struct {
	Title       string
	Description string
	TeamID      string // defaults to the configured team ID
	AssigneeID  string
	StateID     string
	Priority    int // 0=no priority, 1=urgent, 2=high, 3=medium, 4=low
	LabelIDs    []string
}

// CreateIssue creates a new Linear issue and returns the created issue.
func (c *Client) CreateIssue(input CreateIssueInput) (*Issue, error) {
	teamID := input.TeamID
	if teamID == "" {
		teamID = c.teamID
	}
	if teamID == "" {
		return nil, fmt.Errorf("team ID is required — set LINEAR_TEAM_ID or provide team_id in the request")
	}

	query := `
	mutation CreateIssue($input: IssueCreateInput!) {
		issueCreate(input: $input) {
			success
			issue {
				id
				identifier
				title
				description
				url
				state { id name type }
				assignee { id name displayName email }
				priority
				createdAt
				updatedAt
			}
		}
	}`

	variables := map[string]interface{}{
		"input": buildCreateInput(teamID, input),
	}

	var data struct {
		IssueCreate struct {
			Success bool  `json:"success"`
			Issue   Issue `json:"issue"`
		} `json:"issueCreate"`
	}
	if err := c.graphQLRequest(query, variables, &data); err != nil {
		return nil, err
	}
	if !data.IssueCreate.Success {
		return nil, fmt.Errorf("linear issueCreate mutation returned success=false")
	}
	log.Printf("[linear] created issue %s: %s", data.IssueCreate.Issue.Identifier, data.IssueCreate.Issue.URL)
	return &data.IssueCreate.Issue, nil
}

func buildCreateInput(teamID string, input CreateIssueInput) map[string]interface{} {
	m := map[string]interface{}{
		"teamId": teamID,
		"title":  input.Title,
	}
	if input.Description != "" {
		m["description"] = input.Description
	}
	if input.AssigneeID != "" {
		m["assigneeId"] = input.AssigneeID
	}
	if input.StateID != "" {
		m["stateId"] = input.StateID
	}
	if input.Priority != 0 {
		m["priority"] = input.Priority
	}
	if len(input.LabelIDs) > 0 {
		m["labelIds"] = input.LabelIDs
	}
	return m
}

// GetIssue fetches a Linear issue by its identifier (e.g. "ENG-123") or internal UUID.
func (c *Client) GetIssue(identifier string) (*Issue, error) {
	query := `
	query GetIssue($id: String!) {
		issue(id: $id) {
			id
			identifier
			title
			description
			url
			state { id name type }
			assignee { id name displayName email }
			priority
			createdAt
			updatedAt
		}
	}`

	var data struct {
		Issue Issue `json:"issue"`
	}
	if err := c.graphQLRequest(query, map[string]interface{}{"id": identifier}, &data); err != nil {
		return nil, err
	}
	if data.Issue.ID == "" {
		return nil, fmt.Errorf("issue %q not found", identifier)
	}
	return &data.Issue, nil
}

// SearchIssues searches for Linear issues matching the given query string.
func (c *Client) SearchIssues(queryStr string, maxResults int) ([]Issue, error) {
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	if maxResults > maxAllowedResults {
		maxResults = maxAllowedResults
	}

	query := `
	query SearchIssues($filter: IssueFilter, $first: Int) {
		issues(filter: $filter, first: $first, orderBy: updatedAt) {
			nodes {
				id
				identifier
				title
				description
				url
				state { id name type }
				assignee { id name displayName email }
				priority
				createdAt
				updatedAt
			}
		}
	}`

	filter := buildSearchFilter(queryStr, c.teamID)

	variables := map[string]interface{}{
		"filter": filter,
		"first":  maxResults,
	}

	var data struct {
		Issues struct {
			Nodes []Issue `json:"nodes"`
		} `json:"issues"`
	}
	if err := c.graphQLRequest(query, variables, &data); err != nil {
		return nil, err
	}
	return data.Issues.Nodes, nil
}

func buildSearchFilter(queryStr, teamID string) map[string]interface{} {
	filter := map[string]interface{}{}

	if teamID != "" {
		filter["team"] = map[string]interface{}{
			"id": map[string]interface{}{"eq": teamID},
		}
	}
	if queryStr != "" {
		filter["or"] = []interface{}{
			map[string]interface{}{"title": map[string]interface{}{"containsIgnoreCase": queryStr}},
			map[string]interface{}{"description": map[string]interface{}{"containsIgnoreCase": queryStr}},
		}
	}
	return filter
}

// UpdateIssue updates a Linear issue's title and/or description by issue ID or identifier.
func (c *Client) UpdateIssue(issueID, title, description string) error {
	query := `
	mutation UpdateIssue($id: String!, $input: IssueUpdateInput!) {
		issueUpdate(id: $id, input: $input) {
			success
			issue {
				id
				identifier
			}
		}
	}`

	updateInput := map[string]interface{}{}
	if title != "" {
		updateInput["title"] = title
	}
	if description != "" {
		updateInput["description"] = description
	}
	if len(updateInput) == 0 {
		return fmt.Errorf("at least one of title or description must be provided")
	}

	variables := map[string]interface{}{
		"id":    issueID,
		"input": updateInput,
	}

	var data struct {
		IssueUpdate struct {
			Success bool `json:"success"`
			Issue   struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
			} `json:"issue"`
		} `json:"issueUpdate"`
	}
	if err := c.graphQLRequest(query, variables, &data); err != nil {
		return err
	}
	if !data.IssueUpdate.Success {
		return fmt.Errorf("linear issueUpdate mutation returned success=false")
	}
	log.Printf("[linear] updated issue %s", data.IssueUpdate.Issue.Identifier)
	return nil
}

// ListTeams lists all teams accessible to the API token.
func (c *Client) ListTeams() ([]Team, error) {
	query := `
	query ListTeams {
		teams {
			nodes {
				id
				name
				key
				issueCount
			}
		}
	}`

	var data struct {
		Teams struct {
			Nodes []Team `json:"nodes"`
		} `json:"teams"`
	}
	if err := c.graphQLRequest(query, nil, &data); err != nil {
		return nil, err
	}
	return data.Teams.Nodes, nil
}

// ListTeamStates returns the workflow states for the configured (or given) team.
func (c *Client) ListTeamStates(teamID string) ([]State, error) {
	if teamID == "" {
		teamID = c.teamID
	}
	if teamID == "" {
		return nil, fmt.Errorf("team ID is required — set LINEAR_TEAM_ID or provide a team_id")
	}

	query := `
	query ListTeamStates($teamId: String!) {
		team(id: $teamId) {
			states {
				nodes {
					id
					name
					type
				}
			}
		}
	}`

	var data struct {
		Team struct {
			States struct {
				Nodes []State `json:"nodes"`
			} `json:"states"`
		} `json:"team"`
	}
	if err := c.graphQLRequest(query, map[string]interface{}{"teamId": teamID}, &data); err != nil {
		return nil, err
	}
	return data.Team.States.Nodes, nil
}

// SearchMembers searches for Linear users by name or email.
func (c *Client) SearchMembers(query string) ([]User, error) {
	gql := `
	query SearchMembers($filter: UserFilter) {
		users(filter: $filter) {
			nodes {
				id
				name
				displayName
				email
			}
		}
	}`

	filter := map[string]interface{}{
		"or": []interface{}{
			map[string]interface{}{"name": map[string]interface{}{"containsIgnoreCase": query}},
			map[string]interface{}{"displayName": map[string]interface{}{"containsIgnoreCase": query}},
			map[string]interface{}{"email": map[string]interface{}{"containsIgnoreCase": query}},
		},
	}

	var data struct {
		Users struct {
			Nodes []User `json:"nodes"`
		} `json:"users"`
	}
	if err := c.graphQLRequest(gql, map[string]interface{}{"filter": filter}, &data); err != nil {
		return nil, err
	}
	return data.Users.Nodes, nil
}

// DefaultTeamID returns the configured default team ID.
func (c *Client) DefaultTeamID() string {
	return c.teamID
}
