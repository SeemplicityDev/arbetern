package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/aws"
	"github.com/justmike1/arbetern/chorus"
	"github.com/justmike1/arbetern/commands"
	"github.com/justmike1/arbetern/config"
	"github.com/justmike1/arbetern/dashboards"
	dashgitops "github.com/justmike1/arbetern/dashboards/gitopssync"
	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/llm"
	"github.com/justmike1/arbetern/nvd"
	"github.com/justmike1/arbetern/prompts"
	"github.com/justmike1/arbetern/salesforce"
	"github.com/justmike1/arbetern/slack"
	"github.com/justmike1/arbetern/workflows"
	"github.com/justmike1/arbetern/workflows/gitopssync"
)

//go:embed ui/*
var uiFS embed.FS

// ── Integration permission types & cache ────────────────────────────────────

type permission struct {
	Scope       string `json:"scope"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Granted     *bool  `json:"granted,omitempty"` // nil = unknown, true/false = checked
	Extra       bool   `json:"extra,omitempty"`   // true = scope exists on token but not needed by arbetern
}

type integration struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Configured   bool              `json:"configured"`
	AuthMode     string            `json:"auth_mode,omitempty"`
	ActiveModels map[string]string `json:"active_models,omitempty"`
	Permissions  []permission      `json:"permissions"`
}

var (
	integrationsMu    sync.RWMutex
	integrationsCache []integration
)

func boolPtr(v bool) *bool { return &v }

// routerKeys returns the agent IDs from the routers map (for logging).
func routerKeys(m map[string]*commands.Router) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// containsNonBotMentions returns true when text contains Slack user mentions
// (<@U…>) for users other than botUserID — indicating human-to-human
// conversation that the bot should ignore.
func containsNonBotMentions(text, botUserID string) bool {
	for {
		start := strings.Index(text, "<@")
		if start == -1 {
			return false
		}
		text = text[start+2:]
		end := strings.IndexByte(text, '>')
		if end == -1 {
			return false
		}
		uid := text[:end]
		// Handle <@U123|display_name> format.
		if pipe := strings.IndexByte(uid, '|'); pipe >= 0 {
			uid = uid[:pipe]
		}
		if uid != botUserID {
			return true
		}
		text = text[end+1:]
	}
}

// ── RBAC — Slack user group membership check with caching ───────────────────

type groupCacheEntry struct {
	members map[string]bool
	fetched time.Time
}

type groupMemberCache struct {
	mu      sync.RWMutex
	entries map[string]*groupCacheEntry
	ttl     time.Duration
}

func newGroupMemberCache(ttl time.Duration) *groupMemberCache {
	return &groupMemberCache{
		entries: make(map[string]*groupCacheEntry),
		ttl:     ttl,
	}
}

// isMember checks if a user belongs to any of the given Slack user groups.
// Results are cached per group for the configured TTL to avoid API spam.
func (c *groupMemberCache) isMember(slackClient *slack.Client, userID string, groupIDs []string) bool {
	for _, gid := range groupIDs {
		if c.checkGroup(slackClient, userID, gid) {
			return true
		}
	}
	return false
}

func (c *groupMemberCache) checkGroup(slackClient *slack.Client, userID, groupID string) bool {
	c.mu.RLock()
	entry, ok := c.entries[groupID]
	if ok && time.Since(entry.fetched) < c.ttl {
		c.mu.RUnlock()
		return entry.members[userID]
	}
	c.mu.RUnlock()

	// Fetch fresh membership from Slack API.
	members, err := slackClient.GetUserGroupMembers(groupID)
	if err != nil {
		log.Printf("[rbac] failed to fetch members for group %s: %v", groupID, err)
		return false // fail closed — deny on error
	}

	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		memberSet[m] = true
	}

	c.mu.Lock()
	c.entries[groupID] = &groupCacheEntry{members: memberSet, fetched: time.Now()}
	c.mu.Unlock()

	log.Printf("[rbac] refreshed group %s: %d members", groupID, len(members))
	return memberSet[userID]
}

// checkAgentRBAC returns true if the user is allowed to access the agent.
// If the agent has no allowed_teams restriction (empty list), everyone is allowed.
func checkAgentRBAC(cache *groupMemberCache, slackClient *slack.Client, agentID, userID string, allowedTeams []string) bool {
	if len(allowedTeams) == 0 {
		return true // no restriction
	}
	allowed := cache.isMember(slackClient, userID, allowedTeams)
	if !allowed {
		log.Printf("[rbac] DENIED user=%s agent=%s (allowed_teams=%v)", userID, agentID, allowedTeams)
	}
	return allowed
}

const rbacDenyMessage = ":lock: Access denied — you are not a member of an authorized team for this agent. Contact your administrator if you need access."

// Changelog source repository for the /api/changes endpoint.
const (
	changelogOwner = "justmike1"
	changelogRepo  = "arbetern"
)

// buildHelpMessage generates the /arbetern help response from discovered agents.
// Each agent gets a one-line entry extracted from the second line of its intro prompt.
func buildHelpMessage(agents []prompts.AgentConfig) string {
	var b strings.Builder
	b.WriteString("*arbetern* — AI Agent Platform\n\n")
	b.WriteString("*Available agents*\n")
	for _, agent := range agents {
		intro := agent.Prompts["intro"]
		desc := extractIntroLine(intro)
		fmt.Fprintf(&b, "• `/%s` — %s\n", agent.ID, desc)
	}
	b.WriteString("\n*Cross-agent commands*\n")
	b.WriteString("• `/<agent> introduce yourself` — full introduction from any agent\n")
	b.WriteString("• `/arbetern list dashboards` — every active dashboard, grouped by agent\n")
	b.WriteString("• `/arbetern list workflows` — every scheduled/triggered workflow, grouped by agent\n")

	b.WriteString("\n*Create a dashboard* (auto-refreshing read-only view)\n")
	b.WriteString("• `/pulse create dashboard \"Paypal 360\" short-name paypal-360 that syncs every 10m with jira_search, salesforce_query, chorus_list_conversations`\n")
	b.WriteString("• `/ovad create a dashboard of datadog monitors in US alerting for env:prod refreshing every 5m`\n")

	b.WriteString("\n*Create a workflow* (scheduled action — can write / PR / post)\n")
	b.WriteString("• `/ovad create a workflow that every 5m polls jira open bugs in WAK with label arbetern, fixes them in github (PR assigned to claude), and posts to slack channel C02S5BP9LHX`\n")
	b.WriteString("• `/pulse create a workflow every 30m to check datadog monitors alerting for service:payments and DM me the list`\n")
	b.WriteString("• Multi-step (subflows): `/seihin create a workflow every 1h with tasks: 1) list my in-progress tickets, 2) rewrite each description, 3) post a summary to #pm-intake`\n")
	b.WriteString("• Event-triggered: `/agent-q create a workflow that runs on_failure of ovad/<workflow-id> and opens a Jira bug with the error`\n")
	return b.String()
}

// buildDashboardsMessage renders every active dashboard grouped by agent, with
// clickable links to the HTML view. Used by /arbetern list dashboards.
func buildDashboardsMessage(reg *dashboards.Registry, agents []prompts.AgentConfig, appURL string) string {
	var b strings.Builder
	b.WriteString("*arbetern dashboards*\n\n")
	all := reg.List("")
	if len(all) == 0 {
		b.WriteString("_No dashboards are currently registered._\n\n")
		b.WriteString("Ask any agent to build one — e.g.\n")
		b.WriteString("• `/pulse create dashboard \"Paypal 360\" short-name paypal-360 that syncs every 10m with jira_search, salesforce_query, chorus_list_conversations`\n")
		b.WriteString("• `/ovad create a dashboard of datadog monitors alerting for env:prod refreshing every 5m`")
		return b.String()
	}

	// Group by agent, preserving the agent-list order.
	byAgent := make(map[string][]*dashboards.Dashboard, len(agents))
	for _, d := range all {
		byAgent[d.Agent] = append(byAgent[d.Agent], d)
	}

	total := 0
	for _, agent := range agents {
		list := byAgent[agent.ID]
		if len(list) == 0 {
			continue
		}
		fmt.Fprintf(&b, "*/%s* (%d)\n", agent.ID, len(list))
		for _, d := range list {
			label := d.ShortName
			if label == "" {
				label = d.Name
			}
			url := appURL + d.ViewURL()
			lastSync := d.LastSync
			if lastSync == "" {
				lastSync = "pending"
			}
			fmt.Fprintf(&b, "• <%s|%s> — %s (every %s, last sync %s)\n", url, label, d.Name, d.SyncInterval, lastSync)
			total++
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "_%d dashboard%s total._", total, plural(total))
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// buildWorkflowsMessage renders every active workflow grouped by agent, with
// clickable links to the HTML view. Used by /arbetern list workflows.
func buildWorkflowsMessage(reg *workflows.Registry, agents []prompts.AgentConfig, appURL string) string {
	var b strings.Builder
	b.WriteString("*arbetern workflows*\n\n")
	all := reg.List("")
	if len(all) == 0 {
		b.WriteString("_No workflows are currently registered._\n\n")
		b.WriteString("Ask any agent to create one — e.g.\n")
		b.WriteString("• `/ovad create a workflow every 5m: poll jira open bugs in WAK with label arbetern, fix them in github (PR assigned to claude), post to slack C02S5BP9LHX`\n")
		b.WriteString("• Multi-step: `/seihin create a workflow every 1h with tasks: 1) list my in-progress tickets 2) rewrite each description 3) post a summary to #pm-intake`\n")
		b.WriteString("• Event-triggered: `/agent-q create a workflow that runs on_failure of ovad/<id> and opens a Jira bug with the error`")
		return b.String()
	}

	byAgent := make(map[string][]*workflows.Workflow, len(agents))
	for _, w := range all {
		byAgent[w.Agent] = append(byAgent[w.Agent], w)
	}

	total := 0
	for _, agent := range agents {
		list := byAgent[agent.ID]
		if len(list) == 0 {
			continue
		}
		fmt.Fprintf(&b, "*/%s* (%d)\n", agent.ID, len(list))
		for _, w := range list {
			label := w.ShortName
			if label == "" {
				label = w.Name
			}
			url := appURL + w.ViewURL()
			lastRun := w.LastRun
			if lastRun == "" {
				lastRun = "pending"
			}
			status := "ok"
			if w.LastError != "" {
				status = "error"
			}
			fmt.Fprintf(&b, "• <%s|%s> — %s (%s, cron %s, last run %s, %s)\n", url, label, w.Name, w.Pattern(), w.Cron, lastRun, status)
			total++
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "_%d workflow%s total._", total, plural(total))
	return b.String()
}

// workflowExecutor implements workflows.Executor by dispatching to the
// owning agent's Router.RunWorkflow (headless LLM tool loop).
type workflowExecutor struct {
	routers map[string]*commands.Router
}

func (e *workflowExecutor) Run(ctx context.Context, w *workflows.Workflow, prompt string) (string, error) {
	r, ok := e.routers[w.Agent]
	if !ok {
		return "", fmt.Errorf("no router for agent %q", w.Agent)
	}
	return r.RunWorkflow(ctx, w.CreatedBy, prompt)
}

// extractIntroLine returns the second non-empty line from an intro prompt,
// which is typically a one-sentence description of what the agent does.
func extractIntroLine(intro string) string {
	lines := strings.Split(intro, "\n")
	count := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count++
		if count == 2 {
			return line
		}
	}
	return "no description available"
}

// hasScope checks if a scope exists in a granted scopes list.
// For hierarchical scopes like "repo" covering "repo:status", does prefix matching.
// Also handles classic PAT implicit grants (e.g. "repo" implies "actions" and "checks").
func hasScope(granted []string, scope string) bool {
	for _, g := range granted {
		if g == scope {
			return true
		}
		// Hierarchical: "repo" covers "repo:status", "read:org" covers "read:org:xxx".
		if strings.HasPrefix(scope, g+":") || strings.HasPrefix(g, scope+":") {
			return true
		}
	}

	// Classic PAT implicit grants: "repo" includes actions and checks access.
	// These scopes don't appear in X-OAuth-Scopes but are functionally granted.
	repoImplied := map[string]bool{
		"actions":       true,
		"actions:read":  true,
		"actions:write": true,
		"checks":        true,
		"checks:read":   true,
		"checks:write":  true,
	}
	if repoImplied[scope] {
		for _, g := range granted {
			if g == "repo" {
				return true
			}
		}
	}

	return false
}

// refreshIntegrations queries each configured integration's API for live
// permissions and stores the result in the in-memory cache.
func refreshIntegrations(
	cfg *config.Config,
	slackClient *slack.Client,
	ghClient *github.Client,
	jiraClient *atlassian.Client,
	sfClient *salesforce.Client,
	chorusClient *chorus.Client,
	datadogClients *datadog.MultiClient,
	awsClient *aws.Client,
	modelsClient *llm.Client,
	codeModelsClient *llm.Client,
) {
	// --- Slack ---
	slackPerms := []permission{
		{Scope: "chat:write", Description: "Post messages and thread replies in channels", Required: true},
		{Scope: "channels:history", Description: "Read message history in public channels", Required: true},
		{Scope: "groups:history", Description: "Read message history in private channels", Required: true},
		{Scope: "im:history", Description: "Read message history in DMs", Required: false},
		{Scope: "mpim:history", Description: "Read message history in group DMs", Required: false},
		{Scope: "users:read", Description: "Read user profile information (name, email)", Required: true},
		{Scope: "usergroups:read", Description: "Read user group membership for RBAC enforcement", Required: true},
		{Scope: "commands", Description: "Register and receive slash commands", Required: true},
		// Event subscriptions (required for Socket Mode thread follow-ups).
		{Scope: "message.channels", Description: "Event: receive messages in public channels (Socket Mode)", Required: true},
		{Scope: "message.groups", Description: "Event: receive messages in private channels (Socket Mode)", Required: true},
	}
	if cfg.SlackBotToken != "" {
		if scopes, err := slackClient.GetBotScopes(); err == nil && scopes != nil {
			known := make(map[string]bool, len(slackPerms))
			for i := range slackPerms {
				// Event subscriptions (message.channels, message.groups) are not
				// OAuth scopes — they can't be verified via the token. Leave
				// Granted as nil (unknown) for those entries.
				if strings.HasPrefix(slackPerms[i].Scope, "message.") {
					known[slackPerms[i].Scope] = true
					continue
				}
				slackPerms[i].Granted = boolPtr(hasScope(scopes, slackPerms[i].Scope))
				known[slackPerms[i].Scope] = true
			}
			// Append extra scopes the token has that arbetern doesn't need.
			for _, s := range scopes {
				if !known[s] {
					slackPerms = append(slackPerms, permission{
						Scope:   s,
						Granted: boolPtr(true),
						Extra:   true,
					})
				}
			}
		}
	}

	// --- GitHub ---
	ghPerms := []permission{
		{Scope: "repo", Description: "Full access to private and public repositories (read, write, branches, PRs)", Required: true},
		{Scope: "read:user", Description: "Read authenticated user profile", Required: true},
		{Scope: "read:org", Description: "Read organization membership and list repos", Required: true},
		{Scope: "actions:read", Description: "Read workflow runs, jobs, and logs (CI/CD debugging)", Required: false},
		{Scope: "actions:write", Description: "Re-run workflow jobs (rerun failed jobs, rerun all)", Required: false},
		{Scope: "checks:read", Description: "Read check run annotations for detailed CI feedback", Required: false},
	}
	ghAuthMode := ""
	if cfg.GitHubToken != "" {
		ghAuthMode = "Personal Access Token"
		if ghClient != nil {
			if scopes, err := ghClient.GetGrantedScopes(context.Background()); err == nil && scopes != nil {
				known := make(map[string]bool, len(ghPerms))
				for i := range ghPerms {
					ghPerms[i].Granted = boolPtr(hasScope(scopes, ghPerms[i].Scope))
					known[ghPerms[i].Scope] = true
				}
				// Append extra scopes the token has that arbetern doesn't need.
				for _, s := range scopes {
					if !known[s] {
						ghPerms = append(ghPerms, permission{
							Scope:   s,
							Granted: boolPtr(true),
							Extra:   true,
						})
					}
				}
			}
		}
	}

	result := []integration{
		{
			ID:          "slack",
			Name:        "Slack",
			Configured:  cfg.SlackBotToken != "",
			AuthMode:    "Bot Token",
			Permissions: slackPerms,
		},
		{
			ID:          "github",
			Name:        "GitHub",
			Configured:  cfg.GitHubToken != "",
			AuthMode:    ghAuthMode,
			Permissions: ghPerms,
		},
	}

	// --- Jira ---
	if cfg.AtlassianConfigured() {
		jiraConnected := jiraClient != nil && jiraClient.Ready()
		authMode := "Basic Auth"
		if cfg.AtlassianUseOAuth() {
			authMode = "OAuth 2.0"
		}
		jiraPerms := []permission{
			{Scope: "BROWSE_PROJECTS", Description: "View projects, issues, and field metadata", Required: true},
			{Scope: "CREATE_ISSUES", Description: "Create new issues (tickets, stories, bugs)", Required: true},
			{Scope: "EDIT_ISSUES", Description: "Update issue descriptions, fields, and team assignments", Required: true},
			{Scope: "ASSIGN_ISSUES", Description: "Search assignable users and set issue assignees", Required: false},
			{Scope: "BROWSE_USERS", Description: "Search for Jira users by name or email (global permission)", Required: false},
		}
		if cfg.AtlassianUseOAuth() {
			jiraPerms = append(jiraPerms,
				permission{Scope: "read:jira-work", Description: "OAuth scope: read issues, projects, and boards", Required: true, Granted: boolPtr(jiraConnected)},
				permission{Scope: "write:jira-work", Description: "OAuth scope: create and update issues", Required: true, Granted: boolPtr(jiraConnected)},
				permission{Scope: "read:jira-user", Description: "OAuth scope: read user profiles for assignee resolution", Required: true, Granted: boolPtr(jiraConnected)},
			)
		}

		keys := make([]string, 0, 5)
		for _, p := range jiraPerms {
			if p.Scope == strings.ToUpper(p.Scope) {
				keys = append(keys, p.Scope)
			}
		}
		if jiraConnected {
			if grants, err := jiraClient.GetMyPermissions(keys); err == nil {
				known := make(map[string]bool, len(jiraPerms))
				for i := range jiraPerms {
					if g, ok := grants[jiraPerms[i].Scope]; ok {
						jiraPerms[i].Granted = boolPtr(g)
					}
					known[jiraPerms[i].Scope] = true
				}
				// Append extra Jira permissions the user has that arbetern doesn't need.
				for scope, granted := range grants {
					if !known[scope] && granted {
						jiraPerms = append(jiraPerms, permission{
							Scope:   scope,
							Granted: boolPtr(true),
							Extra:   true,
						})
					}
				}
			}
		}

		result = append(result, integration{
			ID:          "jira",
			Name:        "Jira",
			Configured:  jiraConnected,
			AuthMode:    authMode,
			Permissions: jiraPerms,
		})

		// --- Confluence (shares the same Atlassian client) ---
		confluencePerms := []permission{
			{Scope: "read:confluence-content.all", Description: "Search and read Confluence pages and content", Required: true},
			{Scope: "read:confluence-space.summary", Description: "List and browse Confluence spaces", Required: true},
		}
		if cfg.AtlassianUseOAuth() {
			for i := range confluencePerms {
				confluencePerms[i].Granted = boolPtr(jiraConnected)
			}
		} else {
			// Basic Auth inherits all permissions of the account.
			for i := range confluencePerms {
				confluencePerms[i].Granted = boolPtr(jiraConnected)
			}
		}
		result = append(result, integration{
			ID:          "confluence",
			Name:        "Confluence",
			Configured:  jiraConnected,
			AuthMode:    authMode,
			Permissions: confluencePerms,
		})
	} else {
		result = append(result, integration{
			ID:         "jira",
			Name:       "Jira",
			Configured: false,
			Permissions: []permission{
				{Scope: "BROWSE_PROJECTS", Description: "View projects, issues, and field metadata", Required: true},
				{Scope: "CREATE_ISSUES", Description: "Create new issues (tickets, stories, bugs)", Required: true},
				{Scope: "EDIT_ISSUES", Description: "Update issue descriptions, fields, and team assignments", Required: true},
				{Scope: "ASSIGN_ISSUES", Description: "Search assignable users and set issue assignees", Required: false},
			},
		})
		result = append(result, integration{
			ID:         "confluence",
			Name:       "Confluence",
			Configured: false,
			Permissions: []permission{
				{Scope: "read:confluence-content.all", Description: "Search and read Confluence pages and content", Required: true},
				{Scope: "read:confluence-space.summary", Description: "List and browse Confluence spaces", Required: true},
			},
		})
	}

	// --- Azure OpenAI ---
	if cfg.UseAzure() && modelsClient != nil {
		azurePerms := []permission{
			{Scope: "Cognitive Services OpenAI User", Description: "Azure RBAC role for chat completions inference", Required: true, Granted: boolPtr(true)},
		}

		generalModel := modelsClient.Model()
		codeModel := ""
		if codeModelsClient != nil {
			codeModel = codeModelsClient.Model()
		}

		// Build the active models map.
		activeModels := map[string]string{"General": generalModel}
		if codeModel != "" && codeModel != generalModel {
			activeModels["Code"] = codeModel
		}

		// List all accessible models/deployments.
		if models, err := modelsClient.ListModels(context.Background()); err == nil {
			for _, m := range models {
				isGeneral := m == generalModel
				isCode := m == codeModel && codeModel != generalModel
				desc := "Available deployment"
				if isGeneral && isCode {
					desc = "Active deployment (general + code)"
				} else if isGeneral {
					desc = "Active deployment (general)"
				} else if isCode {
					desc = "Active deployment (code)"
				}
				azurePerms = append(azurePerms, permission{
					Scope:       m,
					Description: desc,
					Required:    isGeneral || isCode,
					Granted:     boolPtr(true),
					Extra:       !isGeneral && !isCode,
				})
			}
		}

		result = append(result, integration{
			ID:           "azure-openai",
			Name:         "Azure OpenAI",
			Configured:   true,
			AuthMode:     "API Key",
			ActiveModels: activeModels,
			Permissions:  azurePerms,
		})
	}

	// --- NVD (National Vulnerability Database) ---
	{
		nvdConfigured := cfg.NVDAPIKey != ""
		authMode := "Public (rate-limited)"
		if nvdConfigured {
			authMode = "API Key"
		}
		nvdPerms := []permission{
			{Scope: "cves/2.0", Description: "Look up CVEs by ID (lookup_cve)", Required: true, Granted: boolPtr(true)},
			{Scope: "cves/2.0?keywordSearch", Description: "Search CVEs by keyword (search_cve)", Required: true, Granted: boolPtr(true)},
		}
		if nvdConfigured {
			nvdPerms = append(nvdPerms, permission{
				Scope: "apiKey", Description: "API key grants ~50 requests per 30s rolling window", Required: false, Granted: boolPtr(true),
			})
		} else {
			nvdPerms = append(nvdPerms, permission{
				Scope: "apiKey", Description: "Without API key, limited to ~5 requests per 30s", Required: false, Granted: boolPtr(false),
			})
		}
		result = append(result, integration{
			ID:          "nvd",
			Name:        "NVD",
			Configured:  true,
			AuthMode:    authMode,
			Permissions: nvdPerms,
		})
	}

	// --- Salesforce ---
	if cfg.SalesforceConfigured() {
		sfConnected := sfClient != nil && sfClient.Ready()
		sfPerms := []permission{
			{Scope: "query", Description: "Execute SOQL queries (accounts, opportunities, contacts)", Required: true, Granted: boolPtr(sfConnected)},
			{Scope: "describe", Description: "Describe SObject metadata (fields, types)", Required: true, Granted: boolPtr(sfConnected)},
		}
		// Verify connectivity by checking identity.
		if sfConnected {
			if info, err := sfClient.GetIdentity(); err == nil {
				sfPerms = append(sfPerms, permission{
					Scope:       "identity",
					Description: fmt.Sprintf("Authenticated as %s (%s)", info.DisplayName, info.Username),
					Required:    false,
					Granted:     boolPtr(true),
				})
			}
		}
		result = append(result, integration{
			ID:          "salesforce",
			Name:        "Salesforce",
			Configured:  sfConnected,
			AuthMode:    "OAuth 2.0 (Client Credentials)",
			Permissions: sfPerms,
		})
	} else {
		result = append(result, integration{
			ID:         "salesforce",
			Name:       "Salesforce",
			Configured: false,
			Permissions: []permission{
				{Scope: "query", Description: "Execute SOQL queries (accounts, opportunities, contacts)", Required: true},
				{Scope: "describe", Description: "Describe SObject metadata (fields, types)", Required: true},
			},
		})
	}

	// --- Chorus ---
	if cfg.ChorusConfigured() {
		chorusConnected := chorusClient != nil && chorusClient.Ready()
		chorusPerms := []permission{
			{Scope: "engagements", Description: "List and search Chorus conversations, meetings, and calls via v3 API", Required: true, Granted: boolPtr(chorusConnected)},
			{Scope: "conversations/:id", Description: "Fetch detailed conversation analytics (summary, trackers, action items, deal)", Required: true, Granted: boolPtr(chorusConnected)},
			{Scope: "sales-qualifications", Description: "Extract and retrieve Sales Qualification Framework (MEDDIC) analysis from call transcripts", Required: false, Granted: boolPtr(chorusConnected)},
			{Scope: "sales-qualifications/writeback-crm", Description: "Write back qualification-derived field updates to CRM", Required: false, Granted: boolPtr(chorusConnected)},
		}
		result = append(result, integration{
			ID:          "chorus",
			Name:        "Chorus",
			Configured:  chorusConnected,
			AuthMode:    "API Token",
			Permissions: chorusPerms,
		})
	} else {
		result = append(result, integration{
			ID:         "chorus",
			Name:       "Chorus",
			Configured: false,
			Permissions: []permission{
				{Scope: "engagements", Description: "List and search Chorus conversations, meetings, and calls via v3 API", Required: true},
				{Scope: "conversations/:id", Description: "Fetch detailed conversation analytics", Required: true},
				{Scope: "sales-qualifications", Description: "Extract and retrieve MEDDIC analysis from call transcripts", Required: false},
				{Scope: "sales-qualifications/writeback-crm", Description: "Write back qualification-derived field updates to CRM", Required: false},
			},
		})
	}

	// --- Datadog ---
	if cfg.DatadogConfigured() {
		ddConnected := datadogClients != nil
		ddPerms := []permission{
			{Scope: "logs_read_data", Description: "Search and read log entries via Log Search API", Required: true, Granted: boolPtr(ddConnected)},
			{Scope: "monitors_read", Description: "List and get monitor details, status, and configuration", Required: true, Granted: boolPtr(ddConnected)},
			{Scope: "hosts_read", Description: "List infrastructure hosts and their metadata", Required: true, Granted: boolPtr(ddConnected)},
			{Scope: "dashboards_read", Description: "List and read dashboard definitions and widgets", Required: true, Granted: boolPtr(ddConnected)},
		}
		activeSites := make(map[string]string)
		if datadogClients != nil {
			activeSites["Sites"] = datadogClients.Sites()
		}
		result = append(result, integration{
			ID:           "datadog",
			Name:         "Datadog",
			Configured:   ddConnected,
			AuthMode:     "API Key + App Key (per-site)",
			Permissions:  ddPerms,
			ActiveModels: activeSites,
		})
	} else {
		result = append(result, integration{
			ID:         "datadog",
			Name:       "Datadog",
			Configured: false,
			Permissions: []permission{
				{Scope: "logs_read_data", Description: "Search and read log entries via Log Search API", Required: true},
				{Scope: "monitors_read", Description: "List and get monitor details, status, and configuration", Required: true},
				{Scope: "hosts_read", Description: "List infrastructure hosts and their metadata", Required: true},
				{Scope: "dashboards_read", Description: "List and read dashboard definitions and widgets", Required: true},
			},
		})
	}

	// --- AWS (Cost Explorer) ---
	{
		awsConnected := awsClient != nil
		awsPerms := []permission{
			{Scope: "ce:GetCostAndUsage", Description: "Query daily / monthly cost and usage aggregates with optional group-by (SERVICE, LINKED_ACCOUNT, …)", Required: true, Granted: boolPtr(awsConnected)},
			{Scope: "ce:GetCostForecast", Description: "Forecast upcoming cost (tomorrow through +30 days by default)", Required: true, Granted: boolPtr(awsConnected)},
			{Scope: "ce:GetDimensionValues", Description: "Enumerate valid dimension values (service names, accounts, usage types) for filtering", Required: false, Granted: boolPtr(awsConnected)},
		}
		activeRegion := map[string]string{}
		if awsConnected {
			activeRegion["Signing region"] = awsClient.Region()
		}
		authMode := ""
		if cfg.AWSConfigured() {
			authMode = "SDK default credential chain (env / profile / IRSA / IMDS)"
		}
		result = append(result, integration{
			ID:           "aws",
			Name:         "AWS",
			Configured:   awsConnected,
			AuthMode:     authMode,
			Permissions:  awsPerms,
			ActiveModels: activeRegion,
		})
	}

	integrationsMu.Lock()
	integrationsCache = result
	integrationsMu.Unlock()
	log.Println("Integration permissions refreshed")
}

// startIntegrationsRefresher runs refreshIntegrations once immediately and
// then again every hour in a background goroutine.
func startIntegrationsRefresher(
	cfg *config.Config,
	slackClient *slack.Client,
	ghClient *github.Client,
	jiraClient *atlassian.Client,
	sfClient *salesforce.Client,
	chorusClient *chorus.Client,
	datadogClients *datadog.MultiClient,
	awsClient *aws.Client,
	modelsClient *llm.Client,
	codeModelsClient *llm.Client,
) {
	refreshIntegrations(cfg, slackClient, ghClient, jiraClient, sfClient, chorusClient, datadogClients, awsClient, modelsClient, codeModelsClient)

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			refreshIntegrations(cfg, slackClient, ghClient, jiraClient, sfClient, chorusClient, datadogClients, awsClient, modelsClient, codeModelsClient)
		}
	}()
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	slackClient := slack.NewClient(cfg.SlackBotToken)

	var ghClient *github.Client
	if cfg.GitHubToken != "" {
		ghClient = github.NewClient(cfg.GitHubToken)
	}

	var modelsClient *llm.Client
	var codeModelsClient *llm.Client
	if cfg.UseAzure() {
		modelsClient = llm.NewAzureClient(cfg.AzureEndpoint, cfg.AzureAPIKey, cfg.GeneralModel)
		log.Printf("Using Azure OpenAI backend: %s (general: %s)", cfg.AzureEndpoint, cfg.GeneralModel)
		codeModelsClient = llm.NewAzureClient(cfg.AzureEndpoint, cfg.AzureAPIKey, cfg.CodeModel)
		if cfg.CodeModel != cfg.GeneralModel {
			log.Printf("Code model (Azure): %s", cfg.CodeModel)
		}
	} else {
		modelsClient = llm.NewClient(cfg.GitHubToken, cfg.GeneralModel)
		log.Printf("Using GitHub Models backend (general: %s)", cfg.GeneralModel)
		codeModelsClient = llm.NewClient(cfg.GitHubToken, cfg.CodeModel)
		if cfg.CodeModel != cfg.GeneralModel {
			log.Printf("Code model (GitHub): %s", cfg.CodeModel)
		}
	}

	var jiraClient *atlassian.Client

	// Validate configured models are accessible before proceeding.
	if err := modelsClient.ValidateModel(context.Background()); err != nil {
		log.Fatalf("GENERAL_MODEL validation failed: %v", err)
	}
	log.Printf("GENERAL_MODEL validated: %s", cfg.GeneralModel)
	if cfg.CodeModel != cfg.GeneralModel {
		if err := codeModelsClient.ValidateModel(context.Background()); err != nil {
			log.Fatalf("CODE_MODEL validation failed: %v", err)
		}
		log.Printf("CODE_MODEL validated: %s", cfg.CodeModel)
	}

	if cfg.AtlassianConfigured() {
		if cfg.AtlassianUseOAuth() {
			jiraClient = atlassian.NewOAuthClient(cfg.AtlassianURL, cfg.AtlassianClientID, cfg.AtlassianClientSecret, cfg.JiraProject)
			if jiraClient.Ready() {
				log.Printf("Atlassian integration enabled (OAuth): %s (default project: %s)", cfg.AtlassianURL, cfg.JiraProject)
			} else {
				log.Printf("Atlassian integration configured (OAuth) but not yet connected — retrying in background")
			}
		} else {
			jiraClient = atlassian.NewClient(cfg.AtlassianURL, cfg.AtlassianEmail, cfg.AtlassianAPIToken, cfg.JiraProject)
			log.Printf("Atlassian integration enabled (Basic Auth): %s (default project: %s)", cfg.AtlassianURL, cfg.JiraProject)
		}
	}

	// NVD CVE API client — enables CVE lookup for the security researcher agent.
	var nvdClient *nvd.Client
	if cfg.NVDAPIKey != "" {
		nvdClient = nvd.NewClient(cfg.NVDAPIKey)
		log.Printf("NVD integration enabled (API key set)")
	} else {
		nvdClient = nvd.NewClient("")
		log.Printf("NVD integration enabled (no API key — rate-limited)")
	}

	// Salesforce client — enables SOQL queries for the CS agent (Pulse).
	// If the initial OAuth handshake fails the service continues and the client
	// retries in the background every 5 seconds until it connects.
	var sfClient *salesforce.Client
	if cfg.SalesforceConfigured() {
		sfClient = salesforce.NewClient(cfg.SFConsumerKey, cfg.SFConsumerSecret, cfg.SFLoginURL)
		if sfClient.Ready() {
			log.Printf("Salesforce integration enabled (instance: %s)", sfClient.InstanceURL())
		} else {
			log.Printf("Salesforce integration configured but not yet connected — retrying in background")
		}
	}

	// Chorus (ZoomInfo) client — enables call intelligence and deal momentum for Pulse.
	var chorusClient *chorus.Client
	if cfg.ChorusConfigured() {
		chorusClient = chorus.NewClient(cfg.ChorusAPIToken, cfg.ChorusBaseURL)
		log.Printf("Chorus integration enabled")
	}

	// Datadog multi-client — supports both US (datadoghq.com) and EU (datadoghq.eu) sites.
	var datadogClients *datadog.MultiClient
	if cfg.DatadogConfigured() {
		datadogClients = datadog.NewMultiClient(cfg.DDAPIKeyUS, cfg.DDAppKeyUS, cfg.DDAPIKeyEU, cfg.DDAppKeyEU)
		log.Printf("Datadog integration enabled (sites: %s)", datadogClients.Sites())
	}

	// AWS Cost Explorer client — credentials resolved via the default SDK
	// chain (env vars / AWS_PROFILE / IRSA / IMDS). We only attempt to
	// construct the client when something in the environment looks like
	// AWS credentials, so that local dev without AWS does not spam warning
	// lines. A failed Retrieve() in NewClient is logged and the client is
	// left nil so tools report a clear "not configured" error.
	var awsClient *aws.Client
	if cfg.AWSConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		awsClient, err = aws.NewClient(ctx, cfg.AWSRegion)
		cancel()
		if err != nil {
			log.Printf("AWS integration misconfigured (tools will be unavailable): %v", err)
			awsClient = nil
		} else {
			log.Printf("AWS integration enabled (signing region: %s)", awsClient.Region())
		}
	}

	// Discover agents and register per-agent webhook routes (/<agent>/webhook).
	agents, err := prompts.DiscoverAgents("")
	if err != nil {
		log.Fatalf("failed to discover agents: %v", err)
	}
	if len(agents) == 0 {
		log.Fatal("no agents found in agents/ directory")
	}

	// Start background integration permission refresher (runs once now, then every hour).
	startIntegrationsRefresher(cfg, slackClient, ghClient, jiraClient, sfClient, chorusClient, datadogClients, awsClient, modelsClient, codeModelsClient)

	// Thread session store — enables follow-up replies in threads without /commands.
	sessions := commands.NewSessionStore(cfg.ThreadSessionTTL)
	log.Printf("Thread session TTL: %s", cfg.ThreadSessionTTL)

	// Per-user context store (ephemeral, on local disk — NOT in the
	// persistent mount). Populated after each Slack request and read on
	// subsequent ones so the agent can recognise recurring user topics.
	// Files older than the retention window are garbage collected.
	userContextDir := os.Getenv("USER_CONTEXT_DIR")
	userContextStore := commands.NewUserContextStore(userContextDir)
	log.Printf("User-context store: %s (TTL=%s)", userContextStore.BaseDir(), commands.UserContextTTL)
	userContextStore.StartGC(context.Background(), time.Hour)

	// RBAC: build agentID → allowedTeams map and group membership cache.
	agentRBAC := make(map[string][]string, len(agents))
	for _, agent := range agents {
		agentRBAC[agent.ID] = agent.AllowedTeams
		if len(agent.AllowedTeams) > 0 {
			log.Printf("RBAC: agent %q restricted to teams %v", agent.ID, agent.AllowedTeams)
		}
	}
	rbacCache := newGroupMemberCache(5 * time.Minute)

	// Map of agentID → Router so the events handler can dispatch thread replies.
	routers := make(map[string]*commands.Router, len(agents))

	// Dashboards registry: background sync of LLM-created data dashboards.
	dashDir := cfg.DashboardsDir
	if dashDir == "" {
		dashDir = dashboards.DefaultDir
	}
	dashExec := dashboards.NewExecutor(dashboards.Clients{
		Jira:    jiraClient,
		SF:      sfClient,
		Chorus:  chorusClient,
		Datadog: datadogClients,
		GitHub:  ghClient,
	})
	dashRegistry, err := dashboards.New(dashDir, dashExec)
	if err != nil {
		log.Fatalf("failed to init dashboards registry: %v", err)
	}
	if err := dashRegistry.LoadAll(context.Background()); err != nil {
		log.Printf("warn: failed to load existing dashboards: %v", err)
	}
	// Kick off the scheduled-sync tickers for every dashboard loaded from
	// disk, but do NOT trigger an immediate sync — server boot should only
	// load state into memory and start timers, not hammer every upstream on
	// startup. Ticks will fire on their normal schedule.
	dashRegistry.StartAll(context.Background())
	defer dashRegistry.StopAll()

	// Workflows registry: scheduled LLM tool-loop runs.
	// The executor is wired below once the routers map is populated, so tick
	// goroutines are not started at LoadAll time — we call StartAllEnabled
	// afterwards.
	wfDir := cfg.WorkflowsDir
	if wfDir == "" {
		wfDir = workflows.DefaultDir
	}
	wfRegistry, err := workflows.New(wfDir, nil)
	if err != nil {
		log.Fatalf("failed to init workflows registry: %v", err)
	}
	if err := wfRegistry.LoadAll(context.Background()); err != nil {
		log.Printf("warn: failed to load existing workflows: %v", err)
	}
	defer wfRegistry.StopAll()

	for _, agent := range agents {
		ap, err := prompts.LoadAgent(agent.ID)
		if err != nil {
			log.Fatalf("failed to load prompts for agent %s: %v", agent.ID, err)
		}

		agentID := agent.ID // capture for closure
		router := commands.NewRouter(slackClient, ghClient, modelsClient, codeModelsClient, jiraClient, nvdClient, sfClient, chorusClient, datadogClients, awsClient, dashRegistry, wfRegistry, ap, agent.ID, cfg.AppURL, sessions, cfg.MaxToolRounds, userContextStore)
		routers[agent.ID] = router

		// Wrap router.Handle with RBAC check.
		rbacHandler := func(channelID, userID, text, responseURL string) {
			if !checkAgentRBAC(rbacCache, slackClient, agentID, userID, agentRBAC[agentID]) {
				_ = slack.RespondToURL(responseURL, rbacDenyMessage, true)
				return
			}
			router.Handle(channelID, userID, text, responseURL)
		}
		handler := slack.NewHandler(cfg.SlackSigningSecret, rbacHandler)

		webhookPath := fmt.Sprintf("/%s/webhook", agent.ID)
		http.Handle(webhookPath, handler)
		log.Printf("Registered agent %q at %s", agent.ID, webhookPath)
	}

	// /arbetern help command — lists all available agents with a one-line description.
	helpMessage := buildHelpMessage(agents)
	arbeternHandler := func(channelID, userID, text, responseURL string) {
		trimmed := strings.TrimSpace(text)
		switch {
		case strings.EqualFold(trimmed, "list dashboards"), strings.EqualFold(trimmed, "dashboards"), strings.EqualFold(trimmed, "list-dashboards"):
			log.Printf("[arbetern] user=%s channel=%s listed dashboards", userID, channelID)
			_ = slack.RespondToURL(responseURL, buildDashboardsMessage(dashRegistry, agents, cfg.AppURL), false)
		case strings.EqualFold(trimmed, "list workflows"), strings.EqualFold(trimmed, "workflows"), strings.EqualFold(trimmed, "list-workflows"):
			log.Printf("[arbetern] user=%s channel=%s listed workflows", userID, channelID)
			_ = slack.RespondToURL(responseURL, buildWorkflowsMessage(wfRegistry, agents, cfg.AppURL), false)
		default:
			log.Printf("[arbetern] user=%s channel=%s requested help", userID, channelID)
			_ = slack.RespondToURL(responseURL, helpMessage, false)
		}
	}
	http.Handle("/arbetern/webhook", slack.NewHandler(cfg.SlackSigningSecret, arbeternHandler))
	log.Printf("Registered /arbetern help command at /arbetern/webhook")

	// Socket Mode — connects outbound to Slack for thread reply events.
	// Requires SLACK_APP_TOKEN (xapp-...) with connections:write scope.
	if cfg.SlackAppToken != "" {
		botUserID, err := slackClient.GetBotUserID()
		if err != nil {
			log.Printf("Warning: could not get bot user ID (thread sessions may echo): %v", err)
		} else {
			log.Printf("Bot user ID: %s", botUserID)
		}

		socketListener := slack.NewSocketListener(cfg.SlackAppToken, cfg.SlackBotToken, botUserID,
			// Thread reply handler.
			func(channelID, threadTS, userID, text string) {
				sess := sessions.Lookup(channelID, threadTS)
				if sess == nil {
					return // not a tracked thread
				}

				// Skip if the session is already processing a message.
				// This prevents casual thread chatter from triggering a
				// second concurrent response while the bot is still working.
				if !sess.TryStartProcessing() {
					log.Printf("[session] thread reply ignored (busy): channel=%s thread=%s user=%s",
						channelID, threadTS, userID)
					return
				}
				defer sess.DoneProcessing()

				// Skip messages that @-mention non-bot users — they are
				// human-to-human conversation, not directed at the bot.
				if containsNonBotMentions(text, botUserID) {
					log.Printf("[session] thread reply ignored (human conversation): channel=%s thread=%s user=%s",
						channelID, threadTS, userID)
					return
				}

				log.Printf("[session] thread reply channel=%s thread=%s user=%s text=%q",
					channelID, threadTS, userID, text)
				sess.Router.HandleThreadReply(channelID, threadTS, userID, text)
			},
			// Slash command handler — routes /<agent> commands to the correct router.
			func(command, channelID, userID, text, responseURL string) {
				// command is e.g. "/seihin" — strip the leading slash to get the agent ID.
				agentID := strings.TrimPrefix(command, "/")

				// /arbetern — project help, not an agent.
				if agentID == "arbetern" {
					arbeternHandler(channelID, userID, text, responseURL)
					return
				}

				router, ok := routers[agentID]
				if !ok {
					log.Printf("[socket-mode] unknown agent for command %q (known: %v)", command, routerKeys(routers))
					return
				}

				// RBAC check.
				if !checkAgentRBAC(rbacCache, slackClient, agentID, userID, agentRBAC[agentID]) {
					_ = slack.RespondToURL(responseURL, rbacDenyMessage, true)
					return
				}

				router.Handle(channelID, userID, text, responseURL)
			},
		)
		go socketListener.Start()
		log.Printf("Socket Mode enabled — listening for thread replies")
	} else {
		log.Printf("Warning: SLACK_APP_TOKEN not set — thread session follow-ups disabled")
	}

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Agent management UI (embedded static files) — behind IP whitelist if configured.
	uiContent, _ := fs.Sub(uiFS, "ui")
	uiCIDRs := parseCIDRs(cfg.UIAllowedCIDRs)
	if len(uiCIDRs) > 0 {
		log.Printf("UI IP whitelist enabled: %s", cfg.UIAllowedCIDRs)
	}
	// Per-route IP gating is no longer needed — globalIPGate (installed on
	// the server Handler below) covers every non-exempt path in one place.
	http.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(uiContent))))
	// Favicon — served without IP whitelist so dashboard/workflow viewers can
	// load it regardless of where they're accessing from.
	http.HandleFunc("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		b, err := uiFS.ReadFile("ui/favicon.svg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	// API: list agents with their prompts (read-only, discovered from agents/ directory).
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		agents, err := prompts.DiscoverAgents("")
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to discover agents: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agents)
	})

	// API: UI settings.
	apiMux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		headerTitle := os.Getenv("UI_HEADER")
		if headerTitle == "" {
			headerTitle = "arbetern"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"header": headerTitle})
	})

	// API: integrations — serves cached integration permissions (refreshed hourly).
	apiMux.HandleFunc("/api/integrations", func(w http.ResponseWriter, r *http.Request) {
		integrationsMu.RLock()
		data := integrationsCache
		integrationsMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(data)
	})

	// API: thread session stats (observability).
	apiMux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		active, opened, expired, explicit := sessions.Stats()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"active":        active,
			"total_opened":  opened,
			"total_expired": expired,
			"total_closed":  explicit,
			"session_ttl":   cfg.ThreadSessionTTL.String(),
		})
	})

	// API: latest changes (commits from the arbetern repo).
	apiMux.HandleFunc("/api/changes", func(w http.ResponseWriter, r *http.Request) {
		if ghClient == nil {
			http.Error(w, "GitHub integration not configured", http.StatusServiceUnavailable)
			return
		}
		commits, err := ghClient.ListCommits(r.Context(), changelogOwner, changelogRepo, "", "", time.Time{}, time.Time{}, 20)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to fetch commits: %v", err), http.StatusInternalServerError)
			return
		}
		// Shorten SHAs to 7 chars for the UI's changelog view.
		for i := range commits {
			if len(commits[i].SHA) > 7 {
				commits[i].SHA = commits[i].SHA[:7]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(commits)
	})

	http.Handle("/api/", apiMux)

	// Dashboard viewer routes: /<agent>/dashboard/<id>[/data.json]
	// and API routes under /api/dashboards{,/...}.
	knownAgents := make(map[string]bool, len(agents))
	for _, a := range agents {
		knownAgents[a.ID] = true
	}
	dashRegistry.RegisterRoutes(http.DefaultServeMux, apiMux, knownAgents)
	wfRegistry.RegisterRoutes(http.DefaultServeMux, apiMux, knownAgents)

	// Wire the workflow executor now that routers are built, then kick off
	// tick goroutines for every workflow that was loaded from disk.
	wfRegistry.SetExecutor(&workflowExecutor{routers: routers})
	wfRegistry.StartAllEnabled(context.Background())

	// Optional GitOps sync: reconcile remote workflow descriptors into the
	// registry when WORKFLOWS_GITOPS_REPO is set. See docs/WORKFLOWS_GITOPS.md.
	var wfSyncer *gitopssync.Syncer
	if cfg.WorkflowsGitOpsRepo != "" {
		if ghClient == nil {
			log.Printf("warn: WORKFLOWS_GITOPS_REPO is set but GITHUB_TOKEN is not — gitops sync disabled")
		} else {
			s, err := gitopssync.New(gitopssync.Config{
				Owner:    cfg.WorkflowsGitOpsOwner,
				Repo:     cfg.WorkflowsGitOpsRepo,
				Branch:   cfg.WorkflowsGitOpsBranch,
				BasePath: cfg.WorkflowsGitOpsBasePath,
				Interval: cfg.WorkflowsGitOpsInterval,
				Prune:    cfg.WorkflowsGitOpsPrune,
			}, ghClient, wfRegistry)
			if err != nil {
				log.Printf("warn: gitops sync init failed: %v", err)
			} else {
				wfSyncer = s
				wfSyncer.Start(context.Background())
				defer wfSyncer.Stop()
			}
		}
	}
	_ = wfSyncer

	// Optional GitOps sync for dashboards (mirrors the workflows poller).
	var dashSyncer *dashgitops.Syncer
	if cfg.DashboardsGitOpsRepo != "" {
		if ghClient == nil {
			log.Printf("warn: DASHBOARDS_GITOPS_REPO is set but GITHUB_TOKEN is not — gitops sync disabled")
		} else {
			s, err := dashgitops.New(dashgitops.Config{
				Owner:    cfg.DashboardsGitOpsOwner,
				Repo:     cfg.DashboardsGitOpsRepo,
				Branch:   cfg.DashboardsGitOpsBranch,
				BasePath: cfg.DashboardsGitOpsBasePath,
				Interval: cfg.DashboardsGitOpsInterval,
				Prune:    cfg.DashboardsGitOpsPrune,
			}, ghClient, dashRegistry)
			if err != nil {
				log.Printf("warn: dashboards gitops sync init failed: %v", err)
			} else {
				dashSyncer = s
				dashSyncer.Start(context.Background())
				defer dashSyncer.Stop()
			}
		}
	}
	_ = dashSyncer

	log.Printf("arbetern server starting on :%s", cfg.Port)

	// Global IP gate: when UI_ALLOWED_CIDRS is set, block every path except
	// the two that must stay reachable from outside the allow-list — Slack's
	// slash-command webhooks (one per agent) and the Kubernetes health
	// probe. A new endpoint added in the future is locked down
	// automatically unless explicitly exempted here.
	exemptPaths := []string{"/healthz"}
	for _, a := range agents {
		exemptPaths = append(exemptPaths, "/"+a.ID+"/webhook")
	}
	gatedHandler := globalIPGate(uiCIDRs, exemptPaths, http.DefaultServeMux)
	if len(uiCIDRs) > 0 {
		log.Printf("Global IP gate enabled — only Slack webhooks and /healthz bypass the allow-list")
	}

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      gatedHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 90 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGTERM/SIGINT (Kubernetes pod termination).
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server failed: %v", err)
		}
	}()

	sig := <-shutdown
	log.Printf("received %s — shutting down gracefully (30s deadline)...", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("graceful shutdown failed: %v", err)
	}
	log.Println("server stopped")
}
