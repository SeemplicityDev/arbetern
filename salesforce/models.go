package salesforce

import (
	"fmt"
	"strings"
)

// ── Convenience query helpers ──────────────────────────────────────────────

// Opportunity represents a Salesforce Opportunity record relevant to CS renewal tracking.
type Opportunity struct {
	ID          string
	Name        string
	AccountName string
	StageName   string
	CloseDate   string
	Amount      float64
	Type        string // "New Business", "Renewal", "Expansion", etc.
	OwnerName   string
	IsClosed    bool
	IsWon       bool
}

// Account represents a Salesforce Account record.
type Account struct {
	ID       string
	Name     string
	Type     string // "Customer", "Prospect", etc.
	Industry string
	Owner    string
	Website  string
}

// Contact represents a Salesforce Contact record.
type Contact struct {
	ID          string
	Name        string
	Email       string
	Title       string
	AccountName string
	Phone       string
}

// ParseOpportunities converts raw SOQL query records into Opportunity structs.
func ParseOpportunities(records []map[string]any) []Opportunity {
	var opps []Opportunity
	for _, r := range records {
		opp := Opportunity{
			ID:        getStr(r, "Id"),
			Name:      getStr(r, "Name"),
			StageName: getStr(r, "StageName"),
			CloseDate: getStr(r, "CloseDate"),
			Type:      getStr(r, "Type"),
			IsClosed:  getBool(r, "IsClosed"),
			IsWon:     getBool(r, "IsWon"),
		}
		if amt, ok := r["Amount"].(float64); ok {
			opp.Amount = amt
		}
		// Account.Name is a nested relationship field.
		if acct, ok := r["Account"].(map[string]any); ok {
			opp.AccountName = getStr(acct, "Name")
		}
		// Owner.Name is a nested relationship field.
		if owner, ok := r["Owner"].(map[string]any); ok {
			opp.OwnerName = getStr(owner, "Name")
		}
		opps = append(opps, opp)
	}
	return opps
}

// ParseAccounts converts raw SOQL query records into Account structs.
func ParseAccounts(records []map[string]any) []Account {
	var accounts []Account
	for _, r := range records {
		acct := Account{
			ID:       getStr(r, "Id"),
			Name:     getStr(r, "Name"),
			Type:     getStr(r, "Type"),
			Industry: getStr(r, "Industry"),
			Website:  getStr(r, "Website"),
		}
		if owner, ok := r["Owner"].(map[string]any); ok {
			acct.Owner = getStr(owner, "Name")
		}
		accounts = append(accounts, acct)
	}
	return accounts
}

// ParseContacts converts raw SOQL query records into Contact structs.
func ParseContacts(records []map[string]any) []Contact {
	var contacts []Contact
	for _, r := range records {
		c := Contact{
			ID:    getStr(r, "Id"),
			Name:  getStr(r, "Name"),
			Email: getStr(r, "Email"),
			Title: getStr(r, "Title"),
			Phone: getStr(r, "Phone"),
		}
		if acct, ok := r["Account"].(map[string]any); ok {
			c.AccountName = getStr(acct, "Name")
		}
		contacts = append(contacts, c)
	}
	return contacts
}

// FormatOpportunities returns a Slack-markdown-formatted summary of opportunities.
func FormatOpportunities(opps []Opportunity) string {
	if len(opps) == 0 {
		return "No opportunities found."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d opportunity(ies):\n\n", len(opps))
	for _, o := range opps {
		fmt.Fprintf(&sb, "• *%s*\n", o.Name)
		if o.AccountName != "" {
			fmt.Fprintf(&sb, "  Account: %s\n", o.AccountName)
		}
		fmt.Fprintf(&sb, "  Stage: %s | Close Date: %s", o.StageName, o.CloseDate)
		if o.Amount > 0 {
			fmt.Fprintf(&sb, " | Amount: $%s", formatAmount(o.Amount))
		}
		if o.Type != "" {
			fmt.Fprintf(&sb, " | Type: %s", o.Type)
		}
		if o.OwnerName != "" {
			fmt.Fprintf(&sb, "\n  Owner: %s", o.OwnerName)
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// FormatAccounts returns a Slack-markdown-formatted summary of accounts.
func FormatAccounts(accounts []Account) string {
	if len(accounts) == 0 {
		return "No accounts found."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d account(s):\n\n", len(accounts))
	for _, a := range accounts {
		fmt.Fprintf(&sb, "• *%s*", a.Name)
		if a.Type != "" {
			fmt.Fprintf(&sb, " (%s)", a.Type)
		}
		sb.WriteString("\n")
		if a.Industry != "" {
			fmt.Fprintf(&sb, "  Industry: %s", a.Industry)
		}
		if a.Owner != "" {
			fmt.Fprintf(&sb, " | Owner: %s", a.Owner)
		}
		if a.Website != "" {
			fmt.Fprintf(&sb, " | Website: %s", a.Website)
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// FormatContacts returns a Slack-markdown-formatted summary of contacts.
func FormatContacts(contacts []Contact) string {
	if len(contacts) == 0 {
		return "No contacts found."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d contact(s):\n\n", len(contacts))
	for _, c := range contacts {
		fmt.Fprintf(&sb, "• *%s*", c.Name)
		if c.Title != "" {
			fmt.Fprintf(&sb, " — %s", c.Title)
		}
		sb.WriteString("\n")
		if c.AccountName != "" {
			fmt.Fprintf(&sb, "  Account: %s", c.AccountName)
		}
		if c.Email != "" {
			fmt.Fprintf(&sb, " | Email: %s", c.Email)
		}
		if c.Phone != "" {
			fmt.Fprintf(&sb, " | Phone: %s", c.Phone)
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// ── helpers ────────────────────────────────────────────────────────────────

func getStr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

func getBool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

func formatAmount(amount float64) string {
	if amount >= 1_000_000 {
		return fmt.Sprintf("%.1fM", amount/1_000_000)
	}
	if amount >= 1_000 {
		return fmt.Sprintf("%.0fK", amount/1_000)
	}
	return fmt.Sprintf("%.0f", amount)
}
