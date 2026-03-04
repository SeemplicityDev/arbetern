package chorus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://chorus.ai"

	// Response body size limit for io.LimitReader.
	maxResponseBody = 10 << 20 // 10 MB

	// maxEngagementPages caps how many continuation pages we fetch.
	// Kept low because each page is large and must fit in the LLM context window.
	maxEngagementPages = 3

	// maxFormattedEngagements limits how many engagements are formatted to keep
	// the tool output within LLM context limits.
	maxFormattedEngagements = 30
)

// Client provides access to the Chorus (ZoomInfo) REST API.
// v3 endpoints use token auth; v1 endpoints use JSON:API format.
type Client struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
}

// NewClient creates a Chorus API client. The token is the per-user API token
// from chorus.ai → Personal Settings.
func NewClient(apiToken, baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiToken:   apiToken,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Ready returns true when an API token is configured.
func (c *Client) Ready() bool { return c.apiToken != "" }

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func (c *Client) doGet(path, accept string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	// Chorus API uses a raw token: Authorization:<token> (no Bearer, no Basic).
	// See docs intro: curl -H "Authorization:abcdefghijklmnopqrstuvwxyz" https://chorus.ai/v3/engagements
	req.Header.Set("Authorization", c.apiToken)
	req.Header.Set("Accept", accept)
	return c.execute(req)
}

func (c *Client) doPost(path string, body []byte) ([]byte, error) {
	req, err := http.NewRequest("POST", c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.apiToken)
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("Accept", "application/vnd.api+json")
	return c.execute(req)
}

func (c *Client) execute(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chorus request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read chorus response: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	log.Printf("[chorus] %s %s → %d (%s, %d bytes)", req.Method, req.URL.Path, resp.StatusCode, ct, len(data))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("chorus HTTP %d: %s", resp.StatusCode, truncate(string(data), 500))
	}

	// Detect HTML responses (e.g. web app SPA served instead of API JSON).
	if strings.Contains(ct, "text/html") || (len(data) > 0 && data[0] == '<') {
		return nil, fmt.Errorf("chorus returned HTML instead of JSON (Content-Type: %s) — check CHORUS_BASE_URL is pointing to the API, not the web app", ct)
	}

	return data, nil
}

// ---------------------------------------------------------------------------
// Engagements — GET /v3/engagements  (v3, token auth)
// ---------------------------------------------------------------------------

// Engagement represents a single engagement from the v3 API.
type Engagement struct {
	EngagementID    string               `json:"engagement_id"`
	Subject         string               `json:"subject"`
	AccountName     string               `json:"account_name"`
	DateTime        float64              `json:"date_time"`       // unix timestamp (seconds, fractional)
	Duration        float64              `json:"duration"`        // seconds
	EngagementType  string               `json:"engagement_type"` // "meeting", "unrecorded_meeting", "dialer", etc.
	MeetingSummary  *string              `json:"meeting_summary"` // nullable; contains HTML
	ActionItems     []string             `json:"action_items"`
	Participants    []EngagementParticip `json:"participants"`
	TrackerMatches  []EngagementTracker  `json:"tracker_matches"`
	OpportunityName *string              `json:"opportunity_name"` // nullable
	OpportunityID   *string              `json:"opportunity_id"`   // nullable
	URL             string               `json:"url"`
	UserName        string               `json:"user_name"`  // engagement owner
	UserEmail       string               `json:"user_email"` // engagement owner email
	NoShow          bool                 `json:"no_show"`
	Language        string               `json:"language"`
	Compliance      *string              `json:"compliance"` // nullable
}

// EngagementParticip is a participant in a v3 engagement.
type EngagementParticip struct {
	Name        *string `json:"name"` // nullable
	Email       string  `json:"email"`
	Title       *string `json:"title"`        // nullable
	Type        string  `json:"type"`         // "rep" or "prospect"
	CompanyName *string `json:"company_name"` // nullable
	PersonID    int64   `json:"person_id"`
	UserID      *int64  `json:"user_id"` // nullable (null for prospects)
}

// EngagementTracker is a tracker match within an engagement.
type EngagementTracker struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// engagementsPage is the raw shape of a v3 engagements response.
type engagementsPage struct {
	ContinuationKey string       `json:"continuation_key"`
	Engagements     []Engagement `json:"engagements"`
}

// EngagementFilter controls the query parameters sent to GET /v3/engagements.
// All fields are optional; only non-zero values are added to the request.
type EngagementFilter struct {
	MinDate              string  // ISO-8601 (e.g. 2026-02-01T00:00:00Z)
	MaxDate              string  // ISO-8601
	MinDuration          float64 // seconds
	MaxDuration          float64 // seconds
	EngagementType       string  // "dialer", etc.
	ContentType          string  // "email_opened", etc.
	ParticipantsEmail    string  // email of a participant
	UserID               string  // comma-separated owner user IDs
	TeamID               string  // comma-separated team IDs
	EngagementID         string  // comma-separated engagement IDs
	WithTrackers         bool    // return tracker info
	DispositionConnected bool
	DispositionVoicemail bool
}

// ListEngagements returns engagements from the v3 API matching the given
// filter. It automatically follows continuation_key pagination up to
// maxEngagementPages.
func (c *Client) ListEngagements(filter EngagementFilter) ([]Engagement, error) {
	params := url.Values{}
	if filter.MinDate != "" {
		params.Set("min_date", filter.MinDate)
	}
	if filter.MaxDate != "" {
		params.Set("max_date", filter.MaxDate)
	}
	if filter.MinDuration > 0 {
		params.Set("min_duration", fmt.Sprintf("%.0f", filter.MinDuration))
	}
	if filter.MaxDuration > 0 {
		params.Set("max_duration", fmt.Sprintf("%.0f", filter.MaxDuration))
	}
	if filter.EngagementType != "" {
		params.Set("engagement_type", filter.EngagementType)
	}
	if filter.ContentType != "" {
		params.Set("content_type", filter.ContentType)
	}
	if filter.ParticipantsEmail != "" {
		params.Set("participants_email", filter.ParticipantsEmail)
	}
	if filter.UserID != "" {
		params.Set("user_id", filter.UserID)
	}
	if filter.TeamID != "" {
		params.Set("team_id", filter.TeamID)
	}
	if filter.EngagementID != "" {
		params.Set("engagement_id", filter.EngagementID)
	}
	if filter.WithTrackers {
		params.Set("with_trackers", "true")
	}
	if filter.DispositionConnected {
		params.Set("disposition_connected", "true")
	}
	if filter.DispositionVoicemail {
		params.Set("disposition_voicemail", "true")
	}

	base := "/v3/engagements?" + params.Encode()
	var allEngagements []Engagement
	path := base

	for page := 0; page < maxEngagementPages; page++ {
		data, err := c.doGet(path, "application/json")
		if err != nil {
			return nil, err
		}

		// Diagnostic: log response shape to debug parsing mismatches.
		preview := string(data)
		if len(preview) > 300 {
			preview = preview[:300] + "…"
		}
		log.Printf("[chorus] engagements page %d raw (%d bytes): %s", page+1, len(data), preview)

		// First try our expected struct.
		var pg engagementsPage
		if err := json.Unmarshal(data, &pg); err != nil {
			log.Printf("[chorus] engagements page %d parse error: %v", page+1, err)
			break
		}

		// If typed parsing found 0 engagements but we got a non-trivial
		// response, the JSON key might differ from "engagements". Try to
		// discover the actual array key dynamically.
		if len(pg.Engagements) == 0 && len(data) > 50 {
			pg.Engagements = probeEngagements(data)
			if len(pg.Engagements) > 0 {
				log.Printf("[chorus] engagements page %d: recovered %d items via dynamic key probe", page+1, len(pg.Engagements))
			}
		}

		allEngagements = append(allEngagements, pg.Engagements...)
		log.Printf("[chorus] engagements page %d: %d items (continuation_key=%q)",
			page+1, len(pg.Engagements), truncate(pg.ContinuationKey, 40))

		if pg.ContinuationKey == "" || len(pg.Engagements) == 0 {
			break
		}
		path = base + "&continuation_key=" + url.QueryEscape(pg.ContinuationKey)
	}

	log.Printf("[chorus] listed engagements: %d total", len(allEngagements))
	return allEngagements, nil
}

// probeEngagements tries to find the engagements array inside a response
// whose top-level keys don't match our expected "engagements" key.
// It iterates top-level keys and attempts to unmarshal any array value into
// []Engagement.
func probeEngagements(data []byte) []Engagement {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	// Log top-level keys so we can fix the struct if needed.
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	log.Printf("[chorus] response top-level keys: %v", keys)

	for _, key := range keys {
		v := raw[key]
		if len(v) == 0 || v[0] != '[' {
			continue // not an array
		}
		var engagements []Engagement
		if err := json.Unmarshal(v, &engagements); err == nil && len(engagements) > 0 {
			log.Printf("[chorus] found engagements under key %q (%d items)", key, len(engagements))
			return engagements
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Conversation detail — v1 JSON:API
//   GET /api/v1/conversations/:id       (single)
// ---------------------------------------------------------------------------

// JSON:API response wrapper.
type jsonAPISingle struct {
	Data jsonAPIResource `json:"data"`
}

type jsonAPIResource struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Attributes ConversationAttributes `json:"attributes"`
}

// ConversationAttributes mirrors the v1 conversation response attributes.
type ConversationAttributes struct {
	Name             string        `json:"name"`
	CompanyName      string        `json:"company_name"`
	UserCompanyName  string        `json:"user_company_name"`
	Summary          string        `json:"summary"`
	SummaryError     string        `json:"summary_error"`
	Status           string        `json:"status"`
	Source           string        `json:"source"`
	Language         string        `json:"language"`
	Private          bool          `json:"private"`
	GeneratedSubject string        `json:"generated_subject"`
	CreatedAt        string        `json:"_created_at"`
	ModifiedAt       string        `json:"_modified_at"`
	ActionItems      []string      `json:"action_items"`
	Recap            []string      `json:"recap"`
	TrackerMatch     string        `json:"tracker_match"`
	Account          *Account      `json:"account,omitempty"`
	Deal             *Deal         `json:"deal,omitempty"`
	Owner            *Owner        `json:"owner,omitempty"`
	Recording        *Recording    `json:"recording,omitempty"`
	EmailData        *EmailData    `json:"email,omitempty"`
	Participants     []Participant `json:"participants"`
	Metrics          []Metric      `json:"metrics"`
	Meeting          *MeetingRef   `json:"meeting,omitempty"`
}

// Account is the CRM account linked to the conversation.
type Account struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	ExtID       string `json:"ext_id"`
	Type        string `json:"type"`
	ZICompanyID string `json:"zi_company_id"`
}

// Deal is the CRM deal/opportunity associated with the conversation.
type Deal struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	CloseDate        string  `json:"close_date"`
	CurrentStage     string  `json:"current_stage"`
	InitialStage     string  `json:"initial_stage"`
	InitialAmount    float64 `json:"initial_amount"`
	Size             float64 `json:"size"`
	SizeIncreasedAmt string  `json:"size_increased_amount"`
	StageAdvancement string  `json:"stage_advancement"`
	OnStageSince     string  `json:"on_stage_since"`
	Engaged          string  `json:"engaged"`
}

// Owner is the Chorus user who owns / recorded the conversation.
type Owner struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	PersonID int    `json:"person_id"`
	UserID   int    `json:"user_id"`
}

// Recording contains recording-level analytics.
type Recording struct {
	Duration          float64   `json:"duration"` // seconds
	StartTime         string    `json:"start_time"`
	ScheduleStartTime string    `json:"schedule_start_time"`
	ScheduleEndTime   string    `json:"schedule_end_time"`
	EndReason         string    `json:"end_reason"`
	AudioOnly         bool      `json:"audio_only"`
	Recordable        bool      `json:"recordable"`
	HasMeetingSummary bool      `json:"has_meeting_summary"`
	Trackers          []Tracker `json:"trackers"`
	Clusters          []Cluster `json:"clusters"`
}

// Tracker is a Chorus tracker hit (e.g. "Pricing", "Competitor mention").
type Tracker struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Count   int    `json:"count"`
	Source  string `json:"source"`
	Type    string `json:"type"`
	OwnerID int    `json:"owner_id"`
}

// Cluster represents a speaker segment in the recording.
type Cluster struct {
	ID          int    `json:"id"`
	SpeakerName string `json:"speaker_name"`
	Type        string `json:"type"` // "cust", etc.
	CompanyName string `json:"company_name"`
	DisplayName string `json:"display_name"`
	Title       string `json:"title"`
	UserID      int    `json:"user_id"`
}

// Participant is an attendee on the call.
type Participant struct {
	Name        string `json:"name"`
	Email       string `json:"email"`
	Type        string `json:"type"`
	CompanyName string `json:"company_name"`
	Title       string `json:"title"`
	IsMyTeam    bool   `json:"is_my_team"`
	PersonID    int    `json:"person_id"`
	UserID      int    `json:"user_id"`
	ZIPersonID  int    `json:"zi_person_id"`
}

// Metric is a named numeric metric from the recording.
type Metric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

// MeetingRef links the conversation to a calendar meeting.
type MeetingRef struct {
	ID         string `json:"id"`
	CalendarID string `json:"calendar_id"`
	ICalUID    string `json:"ical_uid"`
	MeetingURL string `json:"meeting_url"`
}

// EmailData holds e-mail-specific data for email-type conversations.
type EmailData struct {
	SentTime  string `json:"sent_time"`
	Body      string `json:"body"`
	Thread    string `json:"thread"`
	Initiator struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"initiator"`
}

// Conversation is the normalised view returned by the client.
type Conversation struct {
	ID         string
	Type       string // "recording" or "email"
	Attributes ConversationAttributes
}

// conversationFields is the set of fields requested from the v1 API so
// responses stay compact but include all data useful for CS workflows.
const conversationFields = "name,account,deal,owner,owner.email,participants," +
	"recording.duration,recording.start_time,recording.trackers," +
	"recording.clusters,status,company_name,_created_at,metrics,meeting.id,source"

// GetConversation fetches detailed data for a single conversation.
func (c *Client) GetConversation(id string) (*Conversation, error) {
	path := fmt.Sprintf("/api/v1/conversations/%s?fields=%s",
		url.PathEscape(id), url.QueryEscape(conversationFields))
	data, err := c.doGet(path, "application/vnd.api+json")
	if err != nil {
		return nil, err
	}
	var resp jsonAPISingle
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse conversation: %w", err)
	}
	log.Printf("[chorus] fetched conversation %s (%q)", id, resp.Data.Attributes.Name)
	return &Conversation{
		ID:         resp.Data.ID,
		Type:       resp.Data.Type,
		Attributes: resp.Data.Attributes,
	}, nil
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

// FormatConversation renders a single conversation for Slack mrkdwn.
func FormatConversation(conv *Conversation) string {
	a := conv.Attributes
	var sb strings.Builder

	fmt.Fprintf(&sb, "📞 *%s*  (ID: %s)\n", a.Name, conv.ID)

	if a.Recording != nil && a.Recording.StartTime != "" {
		if t, err := time.Parse(time.RFC3339, a.Recording.StartTime); err == nil {
			dur := time.Duration(a.Recording.Duration) * time.Second
			fmt.Fprintf(&sb, "📅 %s · %s\n", t.Format("Jan 2, 2006 3:04 PM"), formatDuration(dur))
		}
	}

	if a.Owner != nil {
		fmt.Fprintf(&sb, "👤 Owner: %s", a.Owner.Name)
		if a.Owner.Email != "" {
			fmt.Fprintf(&sb, " (%s)", a.Owner.Email)
		}
		sb.WriteString("\n")
	}

	if a.Summary != "" {
		fmt.Fprintf(&sb, "\n*Summary:* %s\n", a.Summary)
	}

	if len(a.Recap) > 0 {
		sb.WriteString("\n*Recap:*\n")
		for _, r := range a.Recap {
			fmt.Fprintf(&sb, "  • %s\n", r)
		}
	}

	if a.Deal != nil && a.Deal.Name != "" {
		sb.WriteString("\n*Deal:*\n")
		fmt.Fprintf(&sb, "  💰 %s", a.Deal.Name)
		if a.Deal.CurrentStage != "" {
			fmt.Fprintf(&sb, " · Stage: %s", a.Deal.CurrentStage)
		}
		if a.Deal.Size > 0 {
			fmt.Fprintf(&sb, " · %s", formatAmount(a.Deal.Size))
		} else if a.Deal.InitialAmount > 0 {
			fmt.Fprintf(&sb, " · %s", formatAmount(a.Deal.InitialAmount))
		}
		if a.Deal.CloseDate != "" {
			fmt.Fprintf(&sb, " · Close: %s", a.Deal.CloseDate)
		}
		sb.WriteString("\n")
	}

	if a.Account != nil && a.Account.Name != "" {
		fmt.Fprintf(&sb, "\n*Account:* %s\n", a.Account.Name)
	}

	if len(a.Participants) > 0 {
		sb.WriteString("\n*Participants:*\n")
		for _, p := range a.Participants {
			team := " (external)"
			if p.IsMyTeam {
				team = " (internal)"
			}
			fmt.Fprintf(&sb, "  • %s%s", p.Name, team)
			if p.Title != "" {
				fmt.Fprintf(&sb, " — %s", p.Title)
			}
			sb.WriteString("\n")
		}
	}

	if a.Recording != nil && len(a.Recording.Trackers) > 0 {
		sb.WriteString("\n*Trackers:*\n")
		for _, t := range a.Recording.Trackers {
			fmt.Fprintf(&sb, "  • %s (%d)\n", t.Name, t.Count)
		}
	}

	if len(a.ActionItems) > 0 {
		sb.WriteString("\n*Action Items:*\n")
		for _, item := range a.ActionItems {
			fmt.Fprintf(&sb, "  • %s\n", item)
		}
	}

	if len(a.Metrics) > 0 {
		sb.WriteString("\n*Metrics:*\n")
		for _, m := range a.Metrics {
			fmt.Fprintf(&sb, "  • %s: %.1f\n", m.Name, m.Value)
		}
	}

	return sb.String()
}

// FormatEngagements renders a compact summary of engagements for the LLM.
// Keeps only the fields essential for analysis: title, date, duration,
// account, participants (first 5), summary (truncated), top action items
// (first 3), tracker highlights, and opportunity info.
func FormatEngagements(engagements []Engagement) string {
	if len(engagements) == 0 {
		return "No engagements found."
	}

	showing := len(engagements)
	truncated := false
	if showing > maxFormattedEngagements {
		showing = maxFormattedEngagements
		truncated = true
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d engagement(s)", len(engagements))
	if truncated {
		fmt.Fprintf(&sb, " (showing first %d)", showing)
	}
	sb.WriteString(":\n\n")

	for _, e := range engagements[:showing] {
		// Title / type
		title := e.Subject
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&sb, "🎙️ *%s*", title)
		if e.EngagementType != "" {
			fmt.Fprintf(&sb, "  [%s]", e.EngagementType)
		}
		sb.WriteString("\n")

		// Date & duration
		if e.DateTime > 0 {
			t := time.Unix(int64(e.DateTime), 0).UTC()
			fmt.Fprintf(&sb, "   📅 %s", t.Format("Jan 2, 2006 3:04 PM UTC"))
		}
		if e.Duration > 0 {
			dur := time.Duration(e.Duration) * time.Second
			fmt.Fprintf(&sb, " · %s", formatDuration(dur))
		}
		sb.WriteString("\n")

		// Account
		if e.AccountName != "" {
			fmt.Fprintf(&sb, "   🏢 %s\n", e.AccountName)
		}

		// Opportunity
		if e.OpportunityName != nil && *e.OpportunityName != "" {
			fmt.Fprintf(&sb, "   💰 %s", *e.OpportunityName)
			if e.OpportunityID != nil && *e.OpportunityID != "" {
				fmt.Fprintf(&sb, " (ID: %s)", *e.OpportunityID)
			}
			sb.WriteString("\n")
		}

		// Owner
		if e.UserName != "" {
			fmt.Fprintf(&sb, "   👤 %s", e.UserName)
			if e.UserEmail != "" {
				fmt.Fprintf(&sb, " (%s)", e.UserEmail)
			}
			sb.WriteString("\n")
		}

		// Participants (compact, max 5)
		if len(e.Participants) > 0 {
			sb.WriteString("   👥 ")
			limit := len(e.Participants)
			if limit > 5 {
				limit = 5
			}
			for i, p := range e.Participants[:limit] {
				if i > 0 {
					sb.WriteString(", ")
				}
				name := p.Email // fallback to email when name is null
				if p.Name != nil && *p.Name != "" {
					name = *p.Name
				}
				sb.WriteString(name)
				if p.Title != nil && *p.Title != "" {
					fmt.Fprintf(&sb, " (%s)", *p.Title)
				}
				if p.Type == "prospect" {
					sb.WriteString(" [ext]")
				}
			}
			if len(e.Participants) > 5 {
				fmt.Fprintf(&sb, " +%d more", len(e.Participants)-5)
			}
			sb.WriteString("\n")
		}

		// Meeting summary (truncated, strip HTML)
		if e.MeetingSummary != nil && *e.MeetingSummary != "" {
			summary := strings.ReplaceAll(*e.MeetingSummary, "<br>", "\n")
			summary = strings.ReplaceAll(summary, "<br/>", "\n")
			summary = stripHTMLTags(summary)
			fmt.Fprintf(&sb, "   📝 %s\n", truncate(summary, 300))
		}

		// Action items (max 3)
		if len(e.ActionItems) > 0 {
			sb.WriteString("   *Action Items:*\n")
			limit := len(e.ActionItems)
			if limit > 3 {
				limit = 3
			}
			for _, item := range e.ActionItems[:limit] {
				if item != "" {
					fmt.Fprintf(&sb, "     • %s\n", truncate(item, 150))
				}
			}
			if len(e.ActionItems) > 3 {
				fmt.Fprintf(&sb, "     … and %d more\n", len(e.ActionItems)-3)
			}
		}

		// Tracker highlights (max 5)
		if len(e.TrackerMatches) > 0 {
			sb.WriteString("   🏷️ Trackers: ")
			limit := len(e.TrackerMatches)
			if limit > 5 {
				limit = 5
			}
			for i, t := range e.TrackerMatches[:limit] {
				if i > 0 {
					sb.WriteString(", ")
				}
				fmt.Fprintf(&sb, "%s(%d)", t.Name, t.Count)
			}
			if len(e.TrackerMatches) > 5 {
				fmt.Fprintf(&sb, " +%d more", len(e.TrackerMatches)-5)
			}
			sb.WriteString("\n")
		}

		// URL
		if e.URL != "" {
			fmt.Fprintf(&sb, "   🔗 %s\n", e.URL)
		}

		sb.WriteString("\n")
	}

	if truncated {
		fmt.Fprintf(&sb, "… %d more engagement(s) not shown. Narrow the date range for fewer results.\n", len(engagements)-showing)
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Sales Qualifications — v1 JSON:API
//   POST /api/v1/sales-qualifications           (create extraction)
//   GET  /api/v1/sales-qualifications/:id       (get extraction)
//   POST /api/v1/sales-qualifications/actions/writeback-crm
// ---------------------------------------------------------------------------

// SQFAnalysisField is a single field within a Sales Qualification Framework analysis.
type SQFAnalysisField struct {
	FieldID         string `json:"field_id"`
	FieldName       string `json:"field_name"`
	ChangeType      string `json:"change_type"` // ADDED, UPDATED, etc.
	NewValue        string `json:"new_value"`
	PreviousValue   string `json:"previous_value"`
	SupportingQuote string `json:"supporting_quote"`
}

// SQFMeetingNotes holds the meeting notes and next steps from a sales qualification analysis.
type SQFMeetingNotes struct {
	Notes     *SQFAnalysisField `json:"notes,omitempty"`
	NextSteps *SQFAnalysisField `json:"next_steps,omitempty"`
}

// SalesQualificationAttributes mirrors the attributes of a sales_qualification_analysis response.
type SalesQualificationAttributes struct {
	GeneratedAt          string             `json:"generated_at"`
	UserID               int                `json:"user_id"`
	CustomerID           int                `json:"customer_id"`
	RecordingID          string             `json:"recording_id"`
	SQFName              string             `json:"sqf_name"` // e.g. "MEDDIC"
	SQFAnalysis          []SQFAnalysisField `json:"sqf_analysis"`
	ConversationDuration int                `json:"conversation_duration"` // seconds
	MeetingNotes         *SQFMeetingNotes   `json:"meeting_notes,omitempty"`
	OpportunityID        string             `json:"opportunity_id"`
}

// SalesQualification is the normalised view returned by the client.
type SalesQualification struct {
	ID         string
	Type       string // "sales_qualification_analysis"
	Attributes SalesQualificationAttributes
}

type jsonAPISQResponse struct {
	Data struct {
		ID         string                       `json:"id"`
		Type       string                       `json:"type"`
		Attributes SalesQualificationAttributes `json:"attributes"`
	} `json:"data"`
}

// CreateSalesQualification extracts Sales Qualification Framework data (e.g. MEDDIC)
// from a call transcript by recording ID.
// POST /api/v1/sales-qualifications
func (c *Client) CreateSalesQualification(recordingID string) (*SalesQualification, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"data": map[string]interface{}{
			"attributes": map[string]string{"recording_id": recordingID},
			"type":       "sales_qualifications",
		},
	})
	data, err := c.doPost("/api/v1/sales-qualifications", body)
	if err != nil {
		return nil, err
	}
	var resp jsonAPISQResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse sales qualification: %w", err)
	}
	log.Printf("[chorus] created sales qualification for recording %s (framework: %s)", recordingID, resp.Data.Attributes.SQFName)
	return &SalesQualification{
		ID:         resp.Data.ID,
		Type:       resp.Data.Type,
		Attributes: resp.Data.Attributes,
	}, nil
}

// GetSalesQualification retrieves a previously extracted Sales Qualification Framework analysis.
// GET /api/v1/sales-qualifications/:recording_id
func (c *Client) GetSalesQualification(recordingID string) (*SalesQualification, error) {
	path := fmt.Sprintf("/api/v1/sales-qualifications/%s", url.PathEscape(recordingID))
	data, err := c.doGet(path, "application/vnd.api+json")
	if err != nil {
		return nil, err
	}
	var resp jsonAPISQResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse sales qualification: %w", err)
	}
	log.Printf("[chorus] fetched sales qualification for recording %s (framework: %s)", recordingID, resp.Data.Attributes.SQFName)
	return &SalesQualification{
		ID:         resp.Data.ID,
		Type:       resp.Data.Type,
		Attributes: resp.Data.Attributes,
	}, nil
}

// CRMChange represents a single CRM field change for the write-back endpoint.
type CRMChange struct {
	FieldName     string `json:"field_name"`
	NewValue      string `json:"new_value"`
	PreviousValue string `json:"previous_value"`
}

// WritebackCRM writes sales-qualification-derived field updates back to the CRM.
// POST /api/v1/sales-qualifications/actions/writeback-crm
func (c *Client) WritebackCRM(meetingID, objectType, opportunityID string, changes []CRMChange) error {
	body, _ := json.Marshal(map[string]interface{}{
		"data": map[string]interface{}{
			"attributes": map[string]interface{}{
				"meeting_id":     meetingID,
				"crm_changes":    changes,
				"object_type":    objectType,
				"opportunity_id": opportunityID,
			},
			"type": "crm-field-update",
		},
	})
	_, err := c.doPost("/api/v1/sales-qualifications/actions/writeback-crm", body)
	if err != nil {
		return err
	}
	log.Printf("[chorus] wrote back %d CRM changes for meeting %s (opp: %s)", len(changes), meetingID, opportunityID)
	return nil
}

// FormatSalesQualification renders a sales qualification analysis for Slack mrkdwn.
func FormatSalesQualification(sq *SalesQualification) string {
	a := sq.Attributes
	var sb strings.Builder

	fmt.Fprintf(&sb, "📊 *Sales Qualification — %s*  (ID: %s)\n", a.SQFName, sq.ID)

	if a.RecordingID != "" {
		fmt.Fprintf(&sb, "🎙️ Recording: `%s`\n", a.RecordingID)
	}
	if a.OpportunityID != "" {
		fmt.Fprintf(&sb, "💼 Opportunity: `%s`\n", a.OpportunityID)
	}
	if a.ConversationDuration > 0 {
		dur := time.Duration(a.ConversationDuration) * time.Second
		fmt.Fprintf(&sb, "⏱️ Duration: %s\n", formatDuration(dur))
	}
	if a.GeneratedAt != "" {
		fmt.Fprintf(&sb, "🕐 Generated: %s\n", a.GeneratedAt)
	}

	if len(a.SQFAnalysis) > 0 {
		sb.WriteString("\n*Framework Fields:*\n")
		for _, f := range a.SQFAnalysis {
			fmt.Fprintf(&sb, "  • *%s* (%s): %s\n", f.FieldName, f.ChangeType, f.NewValue)
			if f.PreviousValue != "" {
				fmt.Fprintf(&sb, "    Previous: %s\n", f.PreviousValue)
			}
			if f.SupportingQuote != "" {
				fmt.Fprintf(&sb, "    > _%s_\n", truncate(f.SupportingQuote, 200))
			}
		}
	}

	if a.MeetingNotes != nil {
		sb.WriteString("\n*Meeting Notes:*\n")
		if a.MeetingNotes.Notes != nil && a.MeetingNotes.Notes.NewValue != "" {
			fmt.Fprintf(&sb, "  📝 %s\n", a.MeetingNotes.Notes.NewValue)
		}
		if a.MeetingNotes.NextSteps != nil && a.MeetingNotes.NextSteps.NewValue != "" {
			fmt.Fprintf(&sb, "  ➡️ Next Steps: %s\n", a.MeetingNotes.NextSteps.NewValue)
		}
	}

	return sb.String()
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func formatAmount(amount float64) string {
	if amount >= 1_000_000 {
		return fmt.Sprintf("$%.1fM", amount/1_000_000)
	}
	if amount >= 1_000 {
		return fmt.Sprintf("$%.0fK", amount/1_000)
	}
	return fmt.Sprintf("$%.0f", amount)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// stripHTMLTags removes HTML tags from a string.
func stripHTMLTags(s string) string {
	re := regexp.MustCompile(`<[^>]*>`)
	return strings.TrimSpace(re.ReplaceAllString(s, ""))
}
