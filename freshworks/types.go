package freshworks

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// flexString decodes a JSON value that Freshworks may send as either a string
// or a number (e.g. a deal amount), always yielding the textual form.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(string(b))
	return nil
}

func (f flexString) String() string { return string(f) }

// Freshdesk (ticketing) — https://<domain>/api/v2

// Ticket is a Freshdesk support ticket (subset of fields).
type Ticket struct {
	ID              int64                `json:"id"`
	Subject         string               `json:"subject"`
	DescriptionText string               `json:"description_text"`
	Status          int                  `json:"status"`
	Priority        int                  `json:"priority"`
	Type            string               `json:"type"`
	Source          int                  `json:"source"`
	RequesterID     int64                `json:"requester_id"`
	ResponderID     int64                `json:"responder_id"`
	GroupID         int64                `json:"group_id"`
	Tags            []string             `json:"tags"`
	CreatedAt       string               `json:"created_at"`
	UpdatedAt       string               `json:"updated_at"`
	DueBy           string               `json:"due_by"`
	Conversations   []TicketConversation `json:"conversations"`
}

// TicketConversation is a single reply or note on a Freshdesk ticket.
type TicketConversation struct {
	ID        int64  `json:"id"`
	BodyText  string `json:"body_text"`
	Incoming  bool   `json:"incoming"`
	Private   bool   `json:"private"`
	UserID    int64  `json:"user_id"`
	CreatedAt string `json:"created_at"`
}

type ticketSearchResponse struct {
	Results []Ticket `json:"results"`
	Total   int      `json:"total"`
}

// TicketField is a Freshdesk ticket field (system or custom). Custom fields have
// Default=false and a cf_-prefixed Name that is used directly in the search /
// filter query as cf_<name>:'value' (e.g. a "Customer Name" custom field with
// Name "cf_customer_name" is queried as cf_customer_name:'Acme').
type TicketField struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	Default bool   `json:"default"` // true for built-in system fields
}

// Agent is a Freshdesk agent. The display name and email live on the nested
// contact object.
type Agent struct {
	ID      int64 `json:"id"`
	Contact struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"contact"`
}

func freshdeskStatus(code int) string {
	switch code {
	case 2:
		return "Open"
	case 3:
		return "Pending"
	case 4:
		return "Resolved"
	case 5:
		return "Closed"
	default:
		return "Status " + strconv.Itoa(code)
	}
}

func freshdeskPriority(code int) string {
	switch code {
	case 1:
		return "Low"
	case 2:
		return "Medium"
	case 3:
		return "High"
	case 4:
		return "Urgent"
	default:
		return "Priority " + strconv.Itoa(code)
	}
}

func freshdeskSource(code int) string {
	switch code {
	case 1:
		return "Email"
	case 2:
		return "Portal"
	case 3:
		return "Phone"
	case 7:
		return "Chat"
	case 9:
		return "Feedback Widget"
	case 10:
		return "Outbound Email"
	default:
		return "Source " + strconv.Itoa(code)
	}
}

// Freshchat (conversations) — https://<region>.freshchat.com/v2

// ChatConversation is a Freshchat conversation header.
type ChatConversation struct {
	ConversationID  string        `json:"conversation_id"`
	AppID           string        `json:"app_id"`
	ChannelID       string        `json:"channel_id"`
	Status          string        `json:"status"`
	AssignedAgentID string        `json:"assigned_agent_id"`
	AssignedGroupID string        `json:"assigned_group_id"`
	Messages        []ChatMessage `json:"messages"`
}

// ChatMessage is a single message inside a Freshchat conversation.
type ChatMessage struct {
	ID          string            `json:"id"`
	MessageType string            `json:"message_type"`
	ActorType   string            `json:"actor_type"`
	ActorID     string            `json:"actor_id"`
	CreatedTime string            `json:"created_time"`
	Parts       []ChatMessagePart `json:"message_parts"`
}

// ChatMessagePart is one segment of a Freshchat message; only text parts are used.
type ChatMessagePart struct {
	Text *struct {
		Content string `json:"content"`
	} `json:"text"`
}

type chatMessagesResponse struct {
	Messages []ChatMessage `json:"messages"`
}

// Freshworks CRM (sales) — https://<domain>/crm/sales/api

// CRMSearchResult is one hit from the CRM global search.
type CRMSearchResult struct {
	// ID is a flexString because Freshworks returns it as a JSON number for
	// some entity types (contact, deal) and a JSON string for others
	// (sales_account) — decoding into int64 previously failed the whole search.
	ID    flexString `json:"id"`
	Name  string     `json:"name"`
	Type  string     `json:"type"`
	Email string     `json:"email"`
}

// CRMContact is a Freshworks CRM contact (subset of fields).
type CRMContact struct {
	ID          int64  `json:"id"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Mobile      string `json:"mobile_number"`
	Work        string `json:"work_number"`
	JobTitle    string `json:"job_title"`
	City        string `json:"city"`
	Country     string `json:"country"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type contactResponse struct {
	Contact CRMContact `json:"contact"`
}

// CRMDeal is a Freshworks CRM deal/opportunity (subset of fields).
type CRMDeal struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Amount        flexString `json:"amount"`
	BaseCurrency  string     `json:"base_currency_amount"`
	StageID       int64      `json:"deal_stage_id"`
	ExpectedClose string     `json:"expected_close"`
	ClosedDate    string     `json:"closed_date"`
	Probability   flexString `json:"probability"`
	CreatedAt     string     `json:"created_at"`
	UpdatedAt     string     `json:"updated_at"`
}

type dealResponse struct {
	Deal CRMDeal `json:"deal"`
}
