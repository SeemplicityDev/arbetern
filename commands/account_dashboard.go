package commands

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	slacklib "github.com/slack-go/slack"

	"github.com/justmike1/arbetern/dashboards"
	"github.com/justmike1/arbetern/slack"
)

// DashboardSubcommandPrefix is the prefix that triggers the first-class
// account dashboard flow on any agent with Salesforce + one other integration
// configured. Example: "/pulse dashboard Sprout Social --refresh".
const DashboardSubcommandPrefix = "dashboard "

// accountCacheOnce guards a process-wide singleton for the account cache.
// The cache is cheap to construct but the backing directory must exist, and
// creating it once keeps startup side-effects minimal.
var (
	accountCacheOnce sync.Once
	accountCache     *dashboards.AccountCache
	accountCacheErr  error
)

func getAccountCache(root string) (*dashboards.AccountCache, error) {
	accountCacheOnce.Do(func() {
		accountCache, accountCacheErr = dashboards.NewAccountCache(root)
	})
	return accountCache, accountCacheErr
}

// parseDashboardSubcommand inspects a raw slash-command text and returns the
// account name + refresh flag when the text begins with "dashboard ".
// Example inputs and outputs:
//
//	"dashboard Sprout Social"           → ("Sprout Social", false, true)
//	"dashboard Sprout Social --refresh" → ("Sprout Social", true, true)
//	"please create dashboard …"         → ("", false, false)
func parseDashboardSubcommand(text string) (account string, refresh bool, ok bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if !strings.HasPrefix(lower, DashboardSubcommandPrefix) {
		return "", false, false
	}
	rest := strings.TrimSpace(text[len(DashboardSubcommandPrefix):])
	if rest == "" {
		return "", false, false
	}
	// Strip --refresh anywhere in the argument list.
	tokens := strings.Fields(rest)
	kept := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if strings.EqualFold(t, "--refresh") || strings.EqualFold(t, "-r") {
			refresh = true
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == 0 {
		return "", false, false
	}
	return strings.Join(kept, " "), refresh, true
}

// handleAccountDashboard runs the /agent dashboard <account> flow:
//
//  1. Return cached snapshot when fresh and refresh=false.
//  2. Otherwise fan out across all configured integrations (Salesforce, Jira,
//     Chorus, Datadog).
//  3. Compute the health score, persist the dashboard to the registry and the
//     per-account cache, and reply in the audit thread with a Block Kit
//     summary linking to the HTML view.
//
// The command is gated at the router level — only called when the agent has a
// dashboard registry and Salesforce configured.
func (r *Router) handleAccountDashboard(channelID, userID, responseURL, auditTS, account string, refresh bool) {
	log.Printf("[agent=%s user=%s channel=%s] account dashboard request: %q (refresh=%v)",
		r.agentID, userID, channelID, account, refresh)

	// Nudge the user: acknowledge so they see the spinner clear.
	_ = slack.RespondToURL(responseURL, fmt.Sprintf("Building health dashboard for *%s*…", account), true)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	clients := dashboards.Clients{
		Jira:    r.jiraClient,
		SF:      r.sfClient,
		Chorus:  r.chorusClient,
		Datadog: r.datadogClients,
		GitHub:  r.ghClient,
	}

	cache, err := getAccountCache(r.dashboards.Dir())
	if err != nil {
		log.Printf("[account-dashboard] cache init failed: %v", err)
	}

	slug := dashboards.CacheSlug(account)
	var d *dashboards.Dashboard
	var cacheHit bool
	if cache != nil && !refresh {
		if cached, ok := cache.Get(slug); ok {
			d = cached
			cacheHit = true
			log.Printf("[account-dashboard] cache hit for %q", account)
		}
	}

	if d == nil {
		built, err := dashboards.BuildAccountDashboard(ctx, clients, r.agentID, userID, account)
		if err != nil {
			r.postAccountError(channelID, auditTS, account, err)
			return
		}
		d = built
		if cache != nil {
			if err := cache.Put(slug, d); err != nil {
				log.Printf("[account-dashboard] cache persist failed: %v", err)
			}
		}
	}

	// Register under a stable slug-derived ID so the view URL is idempotent
	// across refreshes. Upsert skips the sync goroutine — account dashboards
	// are refreshed on demand by the slash command, not on a ticker.
	d.ID = "acct-" + slug
	if err := r.dashboards.Upsert(d); err != nil {
		log.Printf("[account-dashboard] registry upsert failed: %v", err)
	}

	viewURL := r.appURL + d.ViewURL()
	blocks, fallback := buildAccountBlocks(d, viewURL, cacheHit)

	if auditTS != "" {
		if err := r.slackClient.PostThreadBlocks(channelID, auditTS, fallback, blocks); err != nil {
			log.Printf("[account-dashboard] post blocks failed: %v", err)
			_, _ = r.slackClient.PostMessage(channelID, fallback+"\nView: "+viewURL)
		}
	} else {
		if _, err := r.slackClient.PostBlocks(channelID, fallback, blocks); err != nil {
			log.Printf("[account-dashboard] post blocks failed: %v", err)
			_, _ = r.slackClient.PostMessage(channelID, fallback+"\nView: "+viewURL)
		}
	}
}

func (r *Router) postAccountError(channelID, auditTS, account string, err error) {
	msg := fmt.Sprintf(":warning: Couldn't build a dashboard for *%s*: %s", account, err.Error())
	if auditTS != "" {
		_ = r.slackClient.PostThreadReply(channelID, auditTS, msg)
	} else {
		_, _ = r.slackClient.PostMessage(channelID, msg)
	}
}

// buildAccountBlocks renders the Slack Block Kit summary for a scored
// dashboard. The fallback string is used by clients that can't render blocks
// (mobile notifications, older desktop builds).
func buildAccountBlocks(d *dashboards.Dashboard, viewURL string, cacheHit bool) ([]slacklib.Block, string) {
	acct := d.Account
	if acct == nil {
		return nil, fmt.Sprintf("Dashboard ready: %s", viewURL)
	}

	scoreEmoji := map[string]string{
		"green": ":large_green_circle:", "yellow": ":large_yellow_circle:",
		"orange": ":large_orange_circle:", "red": ":red_circle:",
	}[strings.ToLower(acct.Band)]
	if scoreEmoji == "" {
		scoreEmoji = ":white_circle:"
	}

	header := slacklib.NewHeaderBlock(slacklib.NewTextBlockObject(
		"plain_text",
		fmt.Sprintf("%s — Account Health", acct.AccountName),
		true, false,
	))

	scoreLine := fmt.Sprintf("%s *Score:* `%d / 100`  ·  *Band:* `%s`",
		scoreEmoji, acct.Score, strings.ToUpper(acct.Band))
	if cacheHit {
		scoreLine += "  ·  _cached (add `--refresh` for fresh data)_"
	}
	scoreSection := slacklib.NewSectionBlock(
		slacklib.NewTextBlockObject("mrkdwn", scoreLine, false, false),
		nil, nil,
	)

	risksText := bulletList(acct.Risks, "_No top risks detected._")
	actionsText := bulletList(acct.Actions, "_No immediate action items._")
	twoCol := slacklib.NewSectionBlock(
		nil,
		[]*slacklib.TextBlockObject{
			slacklib.NewTextBlockObject("mrkdwn", "*Top risks*\n"+risksText, false, false),
			slacklib.NewTextBlockObject("mrkdwn", "*Action items*\n"+actionsText, false, false),
		},
		nil,
	)

	// Signal breakdown — show score plus the reasons that drove it so the
	// reader can see *why* each signal landed where it did.
	var sb strings.Builder
	for _, s := range acct.Signals {
		delta := s.Score - s.Weight
		var deltaStr string
		switch {
		case delta < 0:
			deltaStr = fmt.Sprintf(" (`%d`)", delta)
		case delta == 0 && s.Weight > 0:
			deltaStr = " (full credit)"
		}
		fmt.Fprintf(&sb, "• *%s* — `%d / %d`%s\n", s.Name, s.Score, s.Weight, deltaStr)
		for _, reason := range s.Reasons {
			fmt.Fprintf(&sb, "        ◦ %s\n", reason)
		}
	}
	breakdown := slacklib.NewSectionBlock(
		slacklib.NewTextBlockObject("mrkdwn", "*Signal breakdown*\n"+sb.String(), false, false),
		nil, nil,
	)

	button := slacklib.NewButtonBlockElement(
		"open_dashboard",
		viewURL,
		slacklib.NewTextBlockObject("plain_text", "Open full dashboard →", true, false),
	)
	button.URL = viewURL
	button.Style = slacklib.StylePrimary
	actions := slacklib.NewActionBlock("view-actions", button)

	blocks := []slacklib.Block{
		header,
		scoreSection,
		slacklib.NewDividerBlock(),
		twoCol,
		slacklib.NewDividerBlock(),
		breakdown,
		actions,
	}

	fallback := fmt.Sprintf("%s health: %d/100 (%s) — %s",
		acct.AccountName, acct.Score, acct.Band, viewURL)
	return blocks, fallback
}

func bulletList(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "• %s\n", it)
	}
	return b.String()
}
