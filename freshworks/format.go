package freshworks

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// maxListRows caps how many tickets/results a list/search renders inline.
	maxListRows = 20
	// maxBodyChars truncates long ticket descriptions / message bodies.
	maxBodyChars = 800
	// maxConversations caps how many ticket replies/notes are rendered.
	maxConversations = 8
	// maxChatMessages caps how many chat messages are rendered.
	maxChatMessages = 25
	// maxItemBody truncates a single reply / note / chat message body.
	maxItemBody = 350
)

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}

// FormatTicketList renders a slice of tickets as a compact Slack list.
func FormatTicketList(tickets []Ticket, header string) string {
	var sb strings.Builder
	if header == "" {
		header = "Freshdesk tickets"
	}
	fmt.Fprintf(&sb, "*%s — %d result(s)*\n", header, len(tickets))
	if len(tickets) == 0 {
		sb.WriteString("_No tickets found._\n")
		return sb.String()
	}
	limit := len(tickets)
	if limit > maxListRows {
		limit = maxListRows
	}
	for i := 0; i < limit; i++ {
		t := tickets[i]
		fmt.Fprintf(&sb, "• *#%d* %s\n   _%s · %s · updated %s_\n",
			t.ID, truncate(t.Subject, 120),
			freshdeskStatus(t.Status), freshdeskPriority(t.Priority), t.UpdatedAt)
	}
	if len(tickets) > limit {
		fmt.Fprintf(&sb, "_…and %d more._\n", len(tickets)-limit)
	}
	return sb.String()
}

// FormatTicket renders a single ticket with its embedded conversations (if any).
func FormatTicket(t *Ticket) string {
	if t == nil {
		return "No ticket."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Freshdesk ticket #%d — %s*\n", t.ID, truncate(t.Subject, 200))
	fmt.Fprintf(&sb, "Status: *%s* · Priority: *%s* · Source: %s\n",
		freshdeskStatus(t.Status), freshdeskPriority(t.Priority), freshdeskSource(t.Source))
	if t.Type != "" {
		fmt.Fprintf(&sb, "Type: %s\n", t.Type)
	}
	fmt.Fprintf(&sb, "Created: %s · Updated: %s", t.CreatedAt, t.UpdatedAt)
	if t.DueBy != "" {
		fmt.Fprintf(&sb, " · Due: %s", t.DueBy)
	}
	sb.WriteString("\n")
	if len(t.Tags) > 0 {
		fmt.Fprintf(&sb, "Tags: %s\n", strings.Join(t.Tags, ", "))
	}
	if body := truncate(t.DescriptionText, maxBodyChars); body != "" {
		fmt.Fprintf(&sb, "\n%s\n", body)
	}
	if len(t.Conversations) > 0 {
		sb.WriteString("\n*Conversations:*\n")
		sb.WriteString(formatConversations(t.Conversations))
	}
	return sb.String()
}

// FormatTicketConversations renders a slice of ticket replies/notes.
func FormatTicketConversations(ticketID int64, convs []TicketConversation) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Freshdesk ticket #%d — %d conversation(s)*\n", ticketID, len(convs))
	if len(convs) == 0 {
		sb.WriteString("_No conversations._\n")
		return sb.String()
	}
	sb.WriteString(formatConversations(convs))
	return sb.String()
}

// FormatAgents renders resolved Freshdesk agents, highlighting the agent_id to
// use in a ticket search.
func FormatAgents(agents []Agent) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Freshdesk agents — %d match(es)*\n", len(agents))
	if len(agents) == 0 {
		sb.WriteString("_No matching agents._\n")
		return sb.String()
	}
	limit := len(agents)
	if limit > maxListRows {
		limit = maxListRows
	}
	for i := 0; i < limit; i++ {
		a := agents[i]
		name := strings.TrimSpace(a.Contact.Name)
		if name == "" {
			name = "(no name)"
		}
		fmt.Fprintf(&sb, "• *%s* — agent_id `%d`", name, a.ID)
		if a.Contact.Email != "" {
			fmt.Fprintf(&sb, " · %s", a.Contact.Email)
		}
		sb.WriteString("\n")
	}
	if len(agents) > limit {
		fmt.Fprintf(&sb, "_…and %d more._\n", len(agents)-limit)
	}
	sb.WriteString("_Search their tickets with freshdesk_search_tickets query `agent_id:<id>`._\n")
	return sb.String()
}

func formatConversations(convs []TicketConversation) string {
	var sb strings.Builder
	limit := len(convs)
	if limit > maxConversations {
		limit = maxConversations
	}
	for i := 0; i < limit; i++ {
		c := convs[i]
		direction := "Reply"
		if c.Private {
			direction = "Private note"
		} else if c.Incoming {
			direction = "Incoming"
		}
		fmt.Fprintf(&sb, "• _%s · %s_\n   %s\n", direction, c.CreatedAt, truncate(c.BodyText, maxItemBody))
	}
	if len(convs) > limit {
		fmt.Fprintf(&sb, "_…and %d more._\n", len(convs)-limit)
	}
	return sb.String()
}

// Freshchat

// FormatChatConversation renders a Freshchat conversation header (plus any
// inline messages the header response carried).
func FormatChatConversation(c *ChatConversation) string {
	if c == nil {
		return "No conversation."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Freshchat conversation %s*\n", c.ConversationID)
	if c.Status != "" {
		fmt.Fprintf(&sb, "Status: *%s*\n", c.Status)
	}
	if c.ChannelID != "" {
		fmt.Fprintf(&sb, "Channel: %s\n", c.ChannelID)
	}
	if c.AssignedAgentID != "" {
		fmt.Fprintf(&sb, "Assigned agent: %s\n", c.AssignedAgentID)
	}
	if len(c.Messages) > 0 {
		sb.WriteString("\n*Messages:*\n")
		sb.WriteString(formatChatMessages(c.Messages))
	}
	return sb.String()
}

// FormatChatMessages renders a slice of Freshchat messages.
func FormatChatMessages(conversationID string, msgs []ChatMessage) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Freshchat conversation %s — %d message(s)*\n", conversationID, len(msgs))
	if len(msgs) == 0 {
		sb.WriteString("_No messages._\n")
		return sb.String()
	}
	sb.WriteString(formatChatMessages(msgs))
	return sb.String()
}

func formatChatMessages(msgs []ChatMessage) string {
	var sb strings.Builder
	limit := len(msgs)
	if limit > maxChatMessages {
		limit = maxChatMessages
	}
	for i := 0; i < limit; i++ {
		m := msgs[i]
		actor := m.ActorType
		if actor == "" {
			actor = "unknown"
		}
		text := chatMessageText(m)
		if text == "" {
			continue
		}
		fmt.Fprintf(&sb, "• _%s · %s_\n   %s\n", actor, m.CreatedTime, truncate(text, maxItemBody))
	}
	if len(msgs) > limit {
		fmt.Fprintf(&sb, "_…and %d more._\n", len(msgs)-limit)
	}
	return sb.String()
}

func chatMessageText(m ChatMessage) string {
	var parts []string
	for _, p := range m.Parts {
		if p.Text != nil && strings.TrimSpace(p.Text.Content) != "" {
			parts = append(parts, strings.TrimSpace(p.Text.Content))
		}
	}
	return strings.Join(parts, " ")
}

// Freshworks CRM

// FormatCRMSearch renders CRM global-search results.
func FormatCRMSearch(query string, results []CRMSearchResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Freshworks CRM search — %q — %d result(s)*\n", query, len(results))
	if len(results) == 0 {
		sb.WriteString("_No results._\n")
		return sb.String()
	}
	limit := len(results)
	if limit > maxListRows {
		limit = maxListRows
	}
	for i := 0; i < limit; i++ {
		r := results[i]
		line := fmt.Sprintf("• *%s* (%s, id %d)", r.Name, r.Type, r.ID)
		if r.Email != "" {
			line += " — " + r.Email
		}
		sb.WriteString(line + "\n")
	}
	if len(results) > limit {
		fmt.Fprintf(&sb, "_…and %d more._\n", len(results)-limit)
	}
	return sb.String()
}

// FormatCRMContact renders a CRM contact.
func FormatCRMContact(c *CRMContact) string {
	if c == nil {
		return "No contact."
	}
	name := strings.TrimSpace(c.DisplayName)
	if name == "" {
		name = strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Freshworks CRM contact — %s (id %d)*\n", name, c.ID)
	if c.JobTitle != "" {
		fmt.Fprintf(&sb, "Title: %s\n", c.JobTitle)
	}
	if c.Email != "" {
		fmt.Fprintf(&sb, "Email: %s\n", c.Email)
	}
	if c.Mobile != "" || c.Work != "" {
		fmt.Fprintf(&sb, "Phone: %s\n", strings.TrimSpace(strings.Join(nonEmpty(c.Mobile, c.Work), " / ")))
	}
	if loc := strings.TrimSpace(strings.Join(nonEmpty(c.City, c.Country), ", ")); loc != "" {
		fmt.Fprintf(&sb, "Location: %s\n", loc)
	}
	fmt.Fprintf(&sb, "Created: %s · Updated: %s\n", c.CreatedAt, c.UpdatedAt)
	return sb.String()
}

// FormatCRMDeal renders a CRM deal.
func FormatCRMDeal(d *CRMDeal) string {
	if d == nil {
		return "No deal."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Freshworks CRM deal — %s (id %d)*\n", d.Name, d.ID)
	if amt := d.Amount.String(); amt != "" {
		fmt.Fprintf(&sb, "Amount: %s\n", amt)
	}
	if d.StageID != 0 {
		fmt.Fprintf(&sb, "Stage ID: %s\n", strconv.FormatInt(d.StageID, 10))
	}
	if p := d.Probability.String(); p != "" {
		fmt.Fprintf(&sb, "Probability: %s%%\n", p)
	}
	if d.ExpectedClose != "" {
		fmt.Fprintf(&sb, "Expected close: %s\n", d.ExpectedClose)
	}
	if d.ClosedDate != "" {
		fmt.Fprintf(&sb, "Closed: %s\n", d.ClosedDate)
	}
	fmt.Fprintf(&sb, "Created: %s · Updated: %s\n", d.CreatedAt, d.UpdatedAt)
	return sb.String()
}

func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}
