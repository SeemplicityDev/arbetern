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
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/commands"
	"github.com/justmike1/arbetern/config"
	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/nvd"
	"github.com/justmike1/arbetern/prompts"
	"github.com/justmike1/arbetern/salesforce"
	"github.com/justmike1/arbetern/slack"
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

// buildHelpMessage generates the /arbetern help response from discovered agents.
// Each agent gets a one-line entry extracted from the second line of its intro prompt.
func buildHelpMessage(agents []prompts.AgentConfig) string {
	var b strings.Builder
	b.WriteString("*arbetern* — AI Agent Platform\n\n")
	b.WriteString("Available agents:\n")
	for _, agent := range agents {
		intro := agent.Prompts["intro"]
		desc := extractIntroLine(intro)
		fmt.Fprintf(&b, "• `/%s` — %s\n", agent.ID, desc)
	}
	b.WriteString("\nUse `/<agent> introduce yourself` for a full introduction from any agent.")
	return b.String()
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
	modelsClient *github.ModelsClient,
	codeModelsClient *github.ModelsClient,
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

	// --- Jira & Confluence ---
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
				permission{Scope: "read:confluence-content.all", Description: "OAuth scope: read Confluence pages and spaces", Required: false, Granted: boolPtr(jiraConnected)},
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
			ID:          "atlassian",
			Name:        "Atlassian (Jira + Confluence)",
			Configured:  jiraConnected,
			AuthMode:    authMode,
			Permissions: jiraPerms,
		})
	} else {
		result = append(result, integration{
			ID:         "atlassian",
			Name:       "Atlassian (Jira + Confluence)",
			Configured: false,
			Permissions: []permission{
				{Scope: "BROWSE_PROJECTS", Description: "View projects, issues, and field metadata", Required: true},
				{Scope: "CREATE_ISSUES", Description: "Create new issues (tickets, stories, bugs)", Required: true},
				{Scope: "EDIT_ISSUES", Description: "Update issue descriptions, fields, and team assignments", Required: true},
				{Scope: "ASSIGN_ISSUES", Description: "Search assignable users and set issue assignees", Required: false},
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
	modelsClient *github.ModelsClient,
	codeModelsClient *github.ModelsClient,
) {
	refreshIntegrations(cfg, slackClient, ghClient, jiraClient, sfClient, modelsClient, codeModelsClient)

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			refreshIntegrations(cfg, slackClient, ghClient, jiraClient, sfClient, modelsClient, codeModelsClient)
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

	var modelsClient *github.ModelsClient
	var codeModelsClient *github.ModelsClient
	if cfg.UseAzure() {
		modelsClient = github.NewAzureModelsClient(cfg.AzureEndpoint, cfg.AzureAPIKey, cfg.GeneralModel)
		log.Printf("Using Azure OpenAI backend: %s (general: %s)", cfg.AzureEndpoint, cfg.GeneralModel)
		codeModelsClient = github.NewAzureModelsClient(cfg.AzureEndpoint, cfg.AzureAPIKey, cfg.CodeModel)
		if cfg.CodeModel != cfg.GeneralModel {
			log.Printf("Code model (Azure): %s", cfg.CodeModel)
		}
	} else {
		modelsClient = github.NewModelsClient(cfg.GitHubToken, cfg.GeneralModel)
		log.Printf("Using GitHub Models backend (general: %s)", cfg.GeneralModel)
		codeModelsClient = github.NewModelsClient(cfg.GitHubToken, cfg.CodeModel)
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
			jiraClient = atlassian.NewOAuthClient(cfg.AtlassianURL, cfg.AtlassianClientID, cfg.AtlassianClientSecret, cfg.AtlassianProject)
			if jiraClient.Ready() {
				log.Printf("Atlassian integration enabled (OAuth): %s (default project: %s)", cfg.AtlassianURL, cfg.AtlassianProject)
			} else {
				log.Printf("Atlassian integration configured (OAuth) but not yet connected — retrying in background")
			}
		} else {
			jiraClient = atlassian.NewClient(cfg.AtlassianURL, cfg.AtlassianEmail, cfg.AtlassianAPIToken, cfg.AtlassianProject)
			log.Printf("Atlassian integration enabled (Basic Auth): %s (default project: %s)", cfg.AtlassianURL, cfg.AtlassianProject)
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

	// Discover agents and register per-agent webhook routes (/<agent>/webhook).
	agents, err := prompts.DiscoverAgents("")
	if err != nil {
		log.Fatalf("failed to discover agents: %v", err)
	}
	if len(agents) == 0 {
		log.Fatal("no agents found in agents/ directory")
	}

	// Start background integration permission refresher (runs once now, then every hour).
	startIntegrationsRefresher(cfg, slackClient, ghClient, jiraClient, sfClient, modelsClient, codeModelsClient)

	// Thread session store — enables follow-up replies in threads without /commands.
	sessions := commands.NewSessionStore(cfg.ThreadSessionTTL)
	log.Printf("Thread session TTL: %s", cfg.ThreadSessionTTL)

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

	for _, agent := range agents {
		ap, err := prompts.LoadAgent(agent.ID)
		if err != nil {
			log.Fatalf("failed to load prompts for agent %s: %v", agent.ID, err)
		}

		agentID := agent.ID // capture for closure
		router := commands.NewRouter(slackClient, ghClient, modelsClient, codeModelsClient, jiraClient, nvdClient, sfClient, ap, agent.ID, cfg.AppURL, sessions, cfg.MaxToolRounds)
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
	http.Handle("/arbetern/webhook", slack.NewHandler(cfg.SlackSigningSecret, func(channelID, userID, text, responseURL string) {
		log.Printf("[arbetern] user=%s channel=%s requested help", userID, channelID)
		_ = slack.RespondToURL(responseURL, helpMessage, false)
	}))
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
					log.Printf("[socket-mode] user=%s channel=%s requested /arbetern help", userID, channelID)
					_ = slack.RespondToURL(responseURL, helpMessage, false)
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
	uiHandler := ipWhitelist(uiCIDRs, http.StripPrefix("/ui/", http.FileServer(http.FS(uiContent))))
	http.Handle("/ui/", uiHandler)
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

	http.Handle("/api/", ipWhitelist(uiCIDRs, apiMux))

	log.Printf("arbetern server starting on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
