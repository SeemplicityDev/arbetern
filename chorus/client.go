package chorus

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const defaultBaseURL = "https://chorus.ai"

// Client provides access to the Chorus (ZoomInfo) REST API v3.
type Client struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
}

// NewClient creates a Chorus API client.  The token is the per-user API token
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

func (c *Client) doGet(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.apiToken)
	req.Header.Set("Accept", "application/json")
	return c.execute(req)
}

func (c *Client) doPost(path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequest("POST", c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.apiToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	return c.execute(req)
}

func (c *Client) execute(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chorus request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read chorus response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("chorus HTTP %d: %s", resp.StatusCode, truncate(string(data), 500))
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Meetings / Engagements  (GET /api/v3/engagements)
// ---------------------------------------------------------------------------

// Meeting represents a single Chorus engagement (call/meeting).
type Meeting struct {
	ID           string    `json:"meetingId"`
	Title        string    `json:"title"`
	StartTime    time.Time `json:"startTime"`
	Duration     int       `json:"duration"` // seconds
	Participants []string  `json:"participants"`
	ExternalURL  string    `json:"externalUrl"`
}

// ListMeetings returns engagements within the given date window.
// from/to should be RFC-3339 date strings (e.g. "2026-02-01").
func (c *Client) ListMeetings(from, to string, limit int) ([]Meeting, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	path := fmt.Sprintf("/api/v3/engagements?from=%s&to=%s&limit=%d", from, to, limit)
	data, err := c.doGet(path)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Meetings []Meeting `json:"meetings"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse meetings: %w", err)
	}
	log.Printf("[chorus] listed %d meetings (%s → %s)", len(resp.Meetings), from, to)
	return resp.Meetings, nil
}

// ---------------------------------------------------------------------------
// Conversation details  (GET /api/v3/conversations/{meetingId})
// ---------------------------------------------------------------------------

// Conversation holds the detailed analytics for a single meeting.
type Conversation struct {
	MeetingID    string        `json:"meetingId"`
	Title        string        `json:"title"`
	StartTime    time.Time     `json:"startTime"`
	Duration     int           `json:"duration"`
	TalkRatio    float64       `json:"talkRatio"` // 0-1
	Sentiment    string        `json:"sentiment"` // positive / neutral / negative
	Topics       []string      `json:"topics"`
	ActionItems  []ActionItem  `json:"actionItems"`
	Trackers     []Tracker     `json:"trackers"`
	Participants []Participant `json:"participants"`
	Summary      string        `json:"summary"`
	ExternalURL  string        `json:"externalUrl"`
}

// ActionItem is a follow-up captured during the call.
type ActionItem struct {
	Text       string `json:"text"`
	AssignedTo string `json:"assignedTo"`
}

// Tracker is a Chorus tracker hit (e.g. "Pricing", "Competitor mention").
type Tracker struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Participant is an attendee on the call.
type Participant struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"` // internal / external
}

// GetConversation fetches detailed analytics for a single meeting.
func (c *Client) GetConversation(meetingID string) (*Conversation, error) {
	data, err := c.doGet(fmt.Sprintf("/api/v3/conversations/%s", meetingID))
	if err != nil {
		return nil, err
	}
	var conv Conversation
	if err := json.Unmarshal(data, &conv); err != nil {
		return nil, fmt.Errorf("parse conversation: %w", err)
	}
	log.Printf("[chorus] fetched conversation %s (%q)", meetingID, conv.Title)
	return &conv, nil
}

// ---------------------------------------------------------------------------
// Deal Momentum  (POST /api/v3/momentum/deals)
// ---------------------------------------------------------------------------

// Deal represents a deal tracked by Chorus Momentum.
type Deal struct {
	DealID         string    `json:"dealId"`
	Name           string    `json:"name"`
	AccountName    string    `json:"accountName"`
	Amount         float64   `json:"amount"`
	Stage          string    `json:"stage"`
	CloseDate      string    `json:"closeDate"`
	Owner          string    `json:"owner"`
	MomentumScore  float64   `json:"momentumScore"` // 0-100
	LastActivity   time.Time `json:"lastActivity"`
	MeetingCount   int       `json:"meetingCount"`
	EmailCount     int       `json:"emailCount"`
	RiskIndicators []string  `json:"riskIndicators"`
}

// DealsFilter controls which deals are returned.
type DealsFilter struct {
	From  string `json:"from,omitempty"`  // RFC-3339 date
	To    string `json:"to,omitempty"`    // RFC-3339 date
	Owner string `json:"owner,omitempty"` // owner email or name
	Limit int    `json:"limit,omitempty"`
}

// ListDeals returns deals from Chorus Momentum matching the supplied filter.
func (c *Client) ListDeals(filter DealsFilter) ([]Deal, error) {
	if filter.Limit <= 0 || filter.Limit > 100 {
		filter.Limit = 50
	}
	body, err := json.Marshal(filter)
	if err != nil {
		return nil, fmt.Errorf("marshal deals filter: %w", err)
	}
	data, err := c.doPost("/api/v3/momentum/deals", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Deals []Deal `json:"deals"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse deals: %w", err)
	}
	log.Printf("[chorus] listed %d deals", len(resp.Deals))
	return resp.Deals, nil
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

// FormatMeetings renders a compact Slack-friendly summary of meetings.
func FormatMeetings(meetings []Meeting) string {
	if len(meetings) == 0 {
		return "No meetings found."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d meeting(s):\n\n", len(meetings))
	for _, m := range meetings {
		dur := time.Duration(m.Duration) * time.Second
		fmt.Fprintf(&sb, "📞 *%s*\n", m.Title)
		fmt.Fprintf(&sb, "📅 %s · %s", m.StartTime.Format("Jan 2, 2006 3:04 PM"), formatDuration(dur))
		if len(m.Participants) > 0 {
			fmt.Fprintf(&sb, " · %s", strings.Join(m.Participants, ", "))
		}
		if m.ExternalURL != "" {
			fmt.Fprintf(&sb, "\n🔗 <%s|View in Chorus>", m.ExternalURL)
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// FormatConversation renders detailed meeting analytics.
func FormatConversation(conv *Conversation) string {
	var sb strings.Builder
	dur := time.Duration(conv.Duration) * time.Second
	fmt.Fprintf(&sb, "📞 *%s*\n", conv.Title)
	fmt.Fprintf(&sb, "📅 %s · %s\n", conv.StartTime.Format("Jan 2, 2006 3:04 PM"), formatDuration(dur))

	if conv.Summary != "" {
		fmt.Fprintf(&sb, "\n*Summary:* %s\n", conv.Summary)
	}

	if conv.TalkRatio > 0 {
		fmt.Fprintf(&sb, "\n*Talk/Listen Ratio:* %.0f%% / %.0f%%", conv.TalkRatio*100, (1-conv.TalkRatio)*100)
	}
	if conv.Sentiment != "" {
		fmt.Fprintf(&sb, " · *Sentiment:* %s", conv.Sentiment)
	}
	sb.WriteString("\n")

	if len(conv.Participants) > 0 {
		sb.WriteString("\n*Participants:*\n")
		for _, p := range conv.Participants {
			role := ""
			if p.Role != "" {
				role = fmt.Sprintf(" (%s)", p.Role)
			}
			fmt.Fprintf(&sb, "  • %s%s\n", p.Name, role)
		}
	}

	if len(conv.Topics) > 0 {
		fmt.Fprintf(&sb, "\n*Topics:* %s\n", strings.Join(conv.Topics, ", "))
	}

	if len(conv.Trackers) > 0 {
		sb.WriteString("\n*Trackers:*\n")
		for _, t := range conv.Trackers {
			fmt.Fprintf(&sb, "  • %s (%d)\n", t.Name, t.Count)
		}
	}

	if len(conv.ActionItems) > 0 {
		sb.WriteString("\n*Action Items:*\n")
		for _, a := range conv.ActionItems {
			assignee := ""
			if a.AssignedTo != "" {
				assignee = fmt.Sprintf(" → %s", a.AssignedTo)
			}
			fmt.Fprintf(&sb, "  • %s%s\n", a.Text, assignee)
		}
	}

	if conv.ExternalURL != "" {
		fmt.Fprintf(&sb, "\n🔗 <%s|View in Chorus>\n", conv.ExternalURL)
	}

	return sb.String()
}

// FormatDeals renders a compact Slack-friendly summary of deals.
func FormatDeals(deals []Deal) string {
	if len(deals) == 0 {
		return "No deals found."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d deal(s):\n\n", len(deals))
	for _, d := range deals {
		amount := formatAmount(d.Amount)
		fmt.Fprintf(&sb, "💰 *%s* (%s)\n", d.Name, d.AccountName)
		fmt.Fprintf(&sb, "📅 Close: %s · %s · Stage: %s\n", d.CloseDate, amount, d.Stage)
		fmt.Fprintf(&sb, "📊 Momentum: %.0f/100 · 📞 %d calls · ✉️ %d emails\n", d.MomentumScore, d.MeetingCount, d.EmailCount)
		if d.Owner != "" {
			fmt.Fprintf(&sb, "👤 Owner: %s\n", d.Owner)
		}
		if len(d.RiskIndicators) > 0 {
			fmt.Fprintf(&sb, "⚠️ Risk: %s\n", strings.Join(d.RiskIndicators, ", "))
		}
		sb.WriteString("\n")
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
