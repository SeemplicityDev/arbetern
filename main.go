package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/aws"
	"github.com/justmike1/arbetern/azure"
	"github.com/justmike1/arbetern/billing"
	"github.com/justmike1/arbetern/catalog"
	"github.com/justmike1/arbetern/chat"
	"github.com/justmike1/arbetern/chorus"
	"github.com/justmike1/arbetern/clickhouse"
	"github.com/justmike1/arbetern/commands"
	"github.com/justmike1/arbetern/config"
	"github.com/justmike1/arbetern/dashboards"
	dashgitops "github.com/justmike1/arbetern/dashboards/gitopssync"
	"github.com/justmike1/arbetern/databricks"
	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/freshworks"
	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/google"
	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/progress"
	"github.com/justmike1/arbetern/internal/queue"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/vectors"
	"github.com/justmike1/arbetern/llm"
	"github.com/justmike1/arbetern/mcp"
	"github.com/justmike1/arbetern/metrics"
	"github.com/justmike1/arbetern/nvd"
	"github.com/justmike1/arbetern/prompts"
	"github.com/justmike1/arbetern/salesforce"
	"github.com/justmike1/arbetern/skills"
	"github.com/justmike1/arbetern/slack"
	"github.com/justmike1/arbetern/workflows"
	"github.com/justmike1/arbetern/workflows/gitopssync"
)

//go:embed ui/*
var uiFS embed.FS

// uiPages are the client-routed pages served from the SPA shell at /ui/<page>.
var uiPages = map[string]bool{
	"overview":     true,
	"integrations": true,
	"mcp":          true,
	"agents":       true,
	"chats":        true,
	"skills":       true,
	"workflows":    true,
	"dashboards":   true,
	"pulls":        true,
	"tickets":      true,
	"backend":      true,
	"changelog":    true,
	"billing":      true,
	"performance":  true,
	"context":      true,
}

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
	Tools        []string          `json:"tools,omitempty"`
}

// integrationTool is one row of an integration's Tools tab.
type integrationTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// integrationView is the /api/integrations shape: the cached integration with
// its tool names joined to the descriptions the agents' tool loops advertise.
type integrationView struct {
	integration
	Tools []integrationTool `json:"tools,omitempty"`
}

var (
	integrationsMu    sync.RWMutex
	integrationsCache []integration

	toolDescriptionsMu sync.RWMutex
	toolDescriptions   map[string]string
)

// setToolDescriptions collects every tool description the agents can offer.
// Tools of integrations no agent has configured stay undescribed.
func setToolDescriptions(routers map[string]*commands.Router) {
	m := make(map[string]string)
	for _, r := range routers {
		for _, t := range r.ToolDefinitions() {
			if _, seen := m[t.Function.Name]; !seen && t.Function.Description != "" {
				m[t.Function.Name] = t.Function.Description
			}
		}
	}
	toolDescriptionsMu.Lock()
	toolDescriptions = m
	toolDescriptionsMu.Unlock()
}

func describeIntegrations(list []integration) []integrationView {
	toolDescriptionsMu.RLock()
	defer toolDescriptionsMu.RUnlock()
	out := make([]integrationView, 0, len(list))
	for _, ig := range list {
		v := integrationView{integration: ig}
		for _, name := range ig.Tools {
			v.Tools = append(v.Tools, integrationTool{Name: name, Description: toolDescriptions[name]})
		}
		out = append(out, v)
	}
	return out
}

func boolPtr(v bool) *bool { return &v }

// integrationToolNames returns the sorted tool names exposed by the integration
// with the given UI id, for the home page "Tools" tab. Every connector's list
// comes from the single-source commands.ToolsForIntegration (backed by the
// toolIntegration catalogue in commands/tools.go), including the open
// connectors (github/slack/azure). The only special case is the shared
// Atlassian client, whose tools are split across the "jira" and "confluence"
// cards by name.
func integrationToolNames(id string) []string {
	if id == "jira" || id == "confluence" {
		var names []string
		for _, t := range commands.ToolsForIntegration("atlassian") {
			if strings.Contains(t, "confluence") == (id == "confluence") {
				names = append(names, t)
			}
		}
		return names
	}
	return commands.ToolsForIntegration(id)
}

// agentsCacheTTL is how long the discovered agents list is served from memory
// before the next request rebuilds it. Agent discovery walks the agents/
// directory and parses several YAML files per agent, so caching avoids redoing
// that work on every landing-page load.
const agentsCacheTTL = 5 * time.Minute

// agentsCache is a process-wide, thread-safe TTL cache for the /api/agents
// payload. Every viewer shares the same snapshot, so a page refresh just
// re-serves the cached list until the TTL elapses, at which point the next
// request rebuilds it. Concurrent requests during a miss are single-flighted by
// the mutex, and a failed rebuild falls back to the last good snapshot.
type agentsCache struct {
	mu        sync.Mutex
	data      []prompts.AgentConfig
	expiresAt time.Time
}

// get returns the cached agents list, rebuilding it when empty or expired. The
// returned slice is only ever replaced wholesale (never mutated in place), so
// callers may read it after the lock is released.
func (c *agentsCache) get() ([]prompts.AgentConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data != nil && time.Now().Before(c.expiresAt) {
		return c.data, nil
	}
	agents, err := prompts.DiscoverAgents("")
	if err != nil {
		if c.data != nil {
			return c.data, nil // serve the last good snapshot on transient errors
		}
		return nil, err
	}
	c.data = agents
	c.expiresAt = time.Now().Add(agentsCacheTTL)
	return agents, nil
}

// seed installs the roster discovered at startup so the first landing-page
// request is served from memory instead of re-walking the agents directory.
func (c *agentsCache) seed(agents []prompts.AgentConfig) {
	if len(agents) == 0 {
		return
	}
	c.mu.Lock()
	c.data = agents
	c.expiresAt = time.Now().Add(agentsCacheTTL)
	c.mu.Unlock()
}

// changelogCacheTTL is how long the commit list behind the Changelog page is
// served from memory.
const changelogCacheTTL = 5 * time.Minute

const pullsCacheTTL = time.Minute

const ticketsCacheTTL = time.Minute

// agentFromTitle recovers the agent from a conventional "<agent>: subject" PR title.
func agentFromTitle(title string, knownAgents map[string]bool) string {
	prefix, _, ok := strings.Cut(title, ":")
	if !ok {
		return ""
	}
	prefix = strings.TrimSpace(prefix)
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		prefix = prefix[i+1:]
	}
	if knownAgents[prefix] {
		return prefix
	}
	return ""
}

// errGitOpsDisabled is returned by the gitops sync HTTP handler when no
// syncer is configured for that kind (e.g. WORKFLOWS_GITOPS_REPO unset).
var errGitOpsDisabled = errors.New("gitops sync is not enabled for this kind")

const (
	// stateRefreshInterval is how often each replica reconciles its caches
	// with the state bucket.
	stateRefreshInterval = 30 * time.Second
	// schedulerLeaseTTL bounds how long a crashed leader blocks scheduling.
	schedulerLeaseTTL = 30 * time.Second
	// vectorIndexRetry is how often a vector index that failed to open at boot
	// is tried again, so a permission fix takes effect without a restart.
	vectorIndexRetry = 5 * time.Minute
	// catalogSyncInterval is how often the leader re-embeds changed workflow
	// and dashboard descriptors for search.
	catalogSyncInterval = 5 * time.Minute
	// gitOpsStatusStale is how long a stored "running" GitOps status is
	// believed before it is assumed to come from a crashed reconcile.
	gitOpsStatusStale = 10 * time.Minute
	// gitOpsSyncTimeout bounds one out-of-band reconcile started from the UI.
	gitOpsSyncTimeout = 5 * time.Minute
)

type storedGitOpsStatus struct {
	gitopssync.Status
	UpdatedAt time.Time `json:"updated_at"`
}

func gitOpsStatusKey(kind string) string { return "gitops/" + kind + ".json" }

// persistGitOpsStatus shares a syncer's status through the bucket, since only
// the scheduling replica runs the reconcile loop.
func persistGitOpsStatus(b *store.Backend, kind string) func(gitopssync.Status) {
	return func(st gitopssync.Status) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := store.PutJSON(ctx, b, gitOpsStatusKey(kind), storedGitOpsStatus{Status: st, UpdatedAt: time.Now().UTC()}, store.Condition{}); err != nil {
			log.Printf("[gitops] persist %s status: %v", kind, err)
		}
	}
}

// sharedGitOpsStatus serves the status stored by whichever replica last
// reconciled, falling back to this replica's own view.
func sharedGitOpsStatus(b *store.Backend, kind string, local func() any) func() any {
	return func() any {
		own := local()
		if own == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, _, err := store.GetJSON[storedGitOpsStatus](ctx, b, gitOpsStatusKey(kind))
		if err != nil {
			return own
		}
		if st.Running && time.Since(st.UpdatedAt) > gitOpsStatusStale {
			st.Running = false
		}
		return st.Status
	}
}

// registerGitOpsRoutes mounts the per-kind GitOps inspection endpoints on
// apiMux:
//
//	GET  /api/<kindPlural>/_gitops        → status JSON ({enabled:false} if syncer is nil)
//	POST /api/<kindPlural>/_gitops/sync   → trigger an out-of-band reconcile (202)
//
// Both paths are exact matches and therefore take precedence over the
// crud package's `/api/<kindPlural>/` prefix handler.
func registerGitOpsRoutes(apiMux *http.ServeMux, kindPlural string, authorize func(*http.Request) bool, statusFn func() any, syncFn func(context.Context) error) {
	// One reconcile at a time per kind, so a double-click cannot stack
	// goroutines waiting on the syncer's own lock.
	var syncing atomic.Bool
	apiMux.HandleFunc("/api/"+kindPlural+"/_gitops", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		st := statusFn()
		if st == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "status": st})
	})
	apiMux.HandleFunc("/api/"+kindPlural+"/_gitops/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := httpx.CheckSameOrigin(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if authorize != nil && !authorize(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		st := statusFn()
		if st == nil {
			http.Error(w, errGitOpsDisabled.Error(), http.StatusServiceUnavailable)
			return
		}
		// A reconcile clones a repo and rewrites descriptors; it routinely runs
		// for minutes, which is far longer than a proxy will hold a browser
		// request open. Start it and answer at once — the syncer records
		// `running` and the outcome in the shared status the page already
		// polls, so the result survives the request, the tab and this replica.
		started := syncing.CompareAndSwap(false, true)
		if started {
			safego.Go("gitops: sync "+kindPlural, func() {
				defer syncing.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), gitOpsSyncTimeout)
				defer cancel()
				if err := syncFn(ctx); err != nil {
					log.Printf("[gitops] %s manual sync failed: %v", kindPlural, err)
				}
			})
		}
		httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": started, "status": st})
	})
}

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
	// failedAt marks the last refresh that errored. The check runs on every
	// slash command and every gated UI request, so without it a Slack outage
	// means one timed-out lookup per request rather than one per cooldown.
	failedAt time.Time
}

// rbacFailCooldown is how long a failed membership refresh is remembered. It
// only shortens the wait: the answer while it holds is the same denial the
// failed lookup would have produced.
const rbacFailCooldown = 15 * time.Second

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
	recentlyFailed := ok && time.Since(entry.failedAt) < rbacFailCooldown
	c.mu.RUnlock()
	if recentlyFailed {
		return false // fail closed, without waiting on Slack again
	}

	// Fetch fresh membership from Slack API.
	members, err := slackClient.GetUserGroupMembers(groupID)
	if err != nil {
		log.Printf("[rbac] failed to fetch members for group %s: %v", groupID, err)
		c.mu.Lock()
		if cur := c.entries[groupID]; cur != nil {
			cur.failedAt = time.Now()
		} else {
			c.entries[groupID] = &groupCacheEntry{failedAt: time.Now()}
		}
		c.mu.Unlock()
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

// clientEmail extracts the authenticated user's email from the headers an
// upstream OAuth proxy (oauth2-proxy) injects after a successful login.
//
// The headers are only believed when TRUSTED_PROXY_CIDRS is configured and the
// connection came from one of those peers. Without that, anything that can
// reach the pod could name itself an admin, so an untrusted peer is treated as
// unauthenticated. Returns "" for an unauthenticated or untrusted request.
func clientEmail(r *http.Request) string {
	if !identityHeadersTrusted(r) {
		return ""
	}
	for _, h := range []string{"X-Auth-Request-Email", "X-Forwarded-Email"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return strings.ToLower(v)
		}
	}
	return ""
}

// identityHeadersOpen mirrors the legacy behaviour of believing identity
// headers from any peer. It is only enabled when no trusted-proxy list is
// configured, and startup logs a warning whenever that combination is paired
// with an allow-list that the headers can unlock.
var identityHeadersOpen bool

func identityHeadersTrusted(r *http.Request) bool {
	return identityHeadersOpen || fromTrustedProxy(r)
}

// redactEmail masks the local part of an email address so RBAC logs keep the
// domain (the useful part for allow-list debugging) without recording the full
// address in clear text (CWE-312). Returns "<none>" for an empty value and
// "***" for anything not shaped like an address. The "redact" name is also
// recognised by CodeQL's clear-text-logging query as a sanitizer barrier, so
// routing a request-derived email through it before logging clears the alert.
func redactEmail(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return "<none>"
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return "***"
	}
	return email[:1] + "***@" + email[at+1:]
}

// emailAllowed reports whether email matches the allow-list. Entries are
// matched case-insensitively and may be either an exact address
// ("alice@acme.com") or a domain ("acme.com" or "@acme.com") which matches any
// address in that domain.
func emailAllowed(email string, allow []string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	domain := ""
	if at := strings.LastIndex(email, "@"); at != -1 {
		domain = email[at+1:]
	}
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(a, "@")))
		if a == "" {
			continue
		}
		if a == email || a == domain {
			return true
		}
	}
	return false
}

// emailUserIDCache memoises Slack users.lookupByEmail results. The chat-UI
// authorizer resolves the OAuth-proxy email to a Slack user on every request
// to evaluate allowed_teams membership, so both positive and negative lookups
// are cached for a TTL to stay well within Slack's rate limits.
//
// Resolving emails to Slack users requires the users:read.email scope on the
// bot token.
type emailUserIDCache struct {
	mu      sync.RWMutex
	entries map[string]emailUserIDEntry
	ttl     time.Duration
}

type emailUserIDEntry struct {
	userID  string
	fetched time.Time
}

func newEmailUserIDCache(ttl time.Duration) *emailUserIDCache {
	return &emailUserIDCache{entries: make(map[string]emailUserIDEntry), ttl: ttl}
}

// resolve returns the Slack user ID for an email address, or "" when the email
// is empty, no Slack client is configured, the user can't be found, or the
// lookup fails. Results (including misses) are cached for the configured TTL.
func (c *emailUserIDCache) resolve(slackClient *slack.Client, email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || slackClient == nil {
		return ""
	}

	c.mu.RLock()
	entry, ok := c.entries[email]
	if ok && time.Since(entry.fetched) < c.ttl {
		c.mu.RUnlock()
		return entry.userID
	}
	c.mu.RUnlock()

	user, err := slackClient.GetUserByEmail(email)
	userID := ""
	if err != nil {
		log.Printf("[rbac] slack lookup by email %q failed: %v", redactEmail(email), err)
	} else if user != nil {
		userID = user.ID
	}

	c.mu.Lock()
	c.entries[email] = emailUserIDEntry{userID: userID, fetched: time.Now()}
	c.mu.Unlock()
	return userID
}

// slackUserIDRe matches Slack member IDs, the only user keys worth resolving
// to a display name (chat turns are already keyed by email).
var slackUserIDRe = regexp.MustCompile(`^[UW][A-Z0-9]{6,}$`)

type userNameCache struct {
	mu       sync.Mutex
	names    map[string]userNameEntry
	inflight map[string]bool
	ttl      time.Duration
}

type userNameEntry struct {
	name    string
	fetched time.Time
}

func newUserNameCache(ttl time.Duration) *userNameCache {
	return &userNameCache{names: make(map[string]userNameEntry), inflight: make(map[string]bool), ttl: ttl}
}

// resolve returns the cached display name for a Slack user ID, or "" while a
// lookup is pending. Misses are fetched in the background so the summary
// handler never waits on Slack; the name lands on the next refresh.
func (c *userNameCache) resolve(slackClient *slack.Client, id string) string {
	if slackClient == nil || !slackUserIDRe.MatchString(id) {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.names[id]; ok && time.Since(e.fetched) < c.ttl {
		return e.name
	}
	if c.inflight[id] {
		return ""
	}
	c.inflight[id] = true
	safego.Go("billing: resolve user name "+id, func() {
		name := ""
		if user, err := slackClient.GetUserInfo(id); err == nil && user != nil {
			for _, candidate := range []string{user.RealName, user.Profile.RealName, user.Profile.DisplayName} {
				if n := strings.TrimSpace(candidate); n != "" {
					name = n
					break
				}
			}
		} else if err != nil {
			log.Printf("[billing] slack users.info %s failed: %v", id, err)
		}
		c.mu.Lock()
		c.names[id] = userNameEntry{name: name, fetched: time.Now()}
		delete(c.inflight, id)
		c.mu.Unlock()
	})
	return ""
}

type slackIdentity struct {
	ID          string `json:"id"`
	Handle      string `json:"handle,omitempty"`
	RealName    string `json:"real_name,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Title       string `json:"title,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
	Avatar      string `json:"avatar,omitempty"`
}

type atlassianIdentity struct {
	AccountID   string `json:"account_id"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Avatar      string `json:"avatar,omitempty"`
	Site        string `json:"site,omitempty"`
}

// identity is what /api/me returns: the signed-in user's Slack profile, always
// attempted, and their Atlassian account when that integration is connected.
type identity struct {
	Anonymous          bool               `json:"anonymous"`
	Email              string             `json:"email,omitempty"`
	Slack              *slackIdentity     `json:"slack,omitempty"`
	Atlassian          *atlassianIdentity `json:"atlassian,omitempty"`
	AtlassianConnected bool               `json:"atlassian_connected"`
	ResolvedAt         time.Time          `json:"resolved_at"`
}

// identityLinks resolves one stored user-context identity to the others the
// same person is recorded under. Only the Slack direction can be resolved —
// a member ID gives the email, and the email gives the key the console records
// that person under — because the stored email key is a one-way hash.
// Lookups are cached, negatives included, so the background aggregation does
// not call Slack once per person per pass.
type identityLinks struct {
	mu      sync.Mutex
	entries map[string]identityLinkEntry
	ttl     time.Duration
	slack   *slack.Client
}

type identityLinkEntry struct {
	ids     []string
	fetched time.Time
}

func newIdentityLinks(slackClient *slack.Client, ttl time.Duration) *identityLinks {
	return &identityLinks{entries: make(map[string]identityLinkEntry), ttl: ttl, slack: slackClient}
}

func (c *identityLinks) resolve(_ context.Context, id string) []string {
	if c == nil || c.slack == nil || !slackUserIDRe.MatchString(id) {
		return nil
	}
	c.mu.Lock()
	if e, ok := c.entries[id]; ok && time.Since(e.fetched) < c.ttl {
		c.mu.Unlock()
		return e.ids
	}
	c.mu.Unlock()

	var ids []string
	user, err := c.slack.GetUserInfo(id)
	switch {
	case err != nil:
		log.Printf("[user-context] slack users.info %s failed: %v", id, err)
	case user != nil:
		if email := strings.TrimSpace(user.Profile.Email); email != "" {
			ids = []string{commands.UserContextID(email)}
		}
	}
	c.mu.Lock()
	c.entries[id] = identityLinkEntry{ids: ids, fetched: time.Now()}
	c.mu.Unlock()
	return ids
}

type identityCache struct {
	mu      sync.Mutex
	entries map[string]identity
	ttl     time.Duration
}

func newIdentityCache(ttl time.Duration) *identityCache {
	return &identityCache{entries: make(map[string]identity), ttl: ttl}
}

func (c *identityCache) lookup(email string, slackClient *slack.Client, jira *atlassian.Client, site string) identity {
	jiraReady := jira != nil && jira.Ready()
	if email == "" {
		return identity{Anonymous: true, AtlassianConnected: jiraReady}
	}
	c.mu.Lock()
	if e, ok := c.entries[email]; ok && time.Since(e.ResolvedAt) < c.ttl {
		c.mu.Unlock()
		return e
	}
	c.mu.Unlock()

	id := identity{Email: email, AtlassianConnected: jiraReady, ResolvedAt: time.Now()}
	if slackClient != nil {
		if u, err := slackClient.GetUserByEmail(email); err != nil {
			log.Printf("[identity] slack lookup for %s failed: %v", redactEmail(email), err)
		} else if u != nil {
			id.Slack = &slackIdentity{
				ID:          u.ID,
				Handle:      u.Name,
				RealName:    firstNonBlank(u.RealName, u.Profile.RealName),
				DisplayName: u.Profile.DisplayName,
				Title:       u.Profile.Title,
				Timezone:    firstNonBlank(u.TZLabel, u.TZ),
				Avatar:      firstNonBlank(u.Profile.Image72, u.Profile.Image48),
			}
		}
	}
	if jiraReady {
		if users, err := jira.SearchUsersGeneral(email); err != nil {
			log.Printf("[identity] atlassian lookup for %s failed: %v", redactEmail(email), err)
		} else if len(users) > 0 {
			u := users[0]
			for _, cand := range users {
				if strings.EqualFold(cand.EmailAddress, email) {
					u = cand
					break
				}
			}
			id.Atlassian = &atlassianIdentity{AccountID: u.AccountID, DisplayName: u.DisplayName, Email: u.EmailAddress, Avatar: u.AvatarURLs["48x48"], Site: site}
		}
	}
	c.mu.Lock()
	c.entries[email] = id
	c.mu.Unlock()
	return id
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// checkChatRBAC authorizes a chat-UI request for an agent using two layers,
// in priority order:
//
//  1. allowed_emails — the email the OAuth proxy verified matches an exact
//     address or a domain in the list (the primary layer).
//  2. allowed_teams — as a fallback, that email is resolved to a Slack user
//     who is then checked for membership in the agent's authorized Slack
//     teams, reusing the RBAC the Slack bot already enforces.
//
// Access is granted if either layer matches and denied only when neither does.
// When both lists are empty the agent's chat is unrestricted. Team resolution
// fails closed: if the email can't be mapped to a Slack user (e.g. missing the
// users:read.email scope) the fallback simply doesn't grant access.
func checkChatRBAC(r *http.Request, scope string, allowedEmails, allowedTeams []string, slackClient *slack.Client, emailCache *emailUserIDCache, groupCache *groupMemberCache) bool {
	if uiRBACAllowed(r, allowedEmails, allowedTeams, slackClient, emailCache, groupCache) {
		return true
	}
	log.Printf("[rbac] DENIED email=%q scope=%s (allowed_emails=%v allowed_teams=%v)", redactEmail(clientEmail(r)), scope, allowedEmails, allowedTeams)
	return false
}

// uiRBACAllowed is the check behind checkChatRBAC without the denial log, for
// callers that only describe what the viewer may do.
func uiRBACAllowed(r *http.Request, allowedEmails, allowedTeams []string, slackClient *slack.Client, emailCache *emailUserIDCache, groupCache *groupMemberCache) bool {
	if len(allowedEmails) == 0 && len(allowedTeams) == 0 {
		return true // no restriction
	}

	email := clientEmail(r)

	// Layer 1: email / domain allow-list (highest priority).
	if len(allowedEmails) > 0 && emailAllowed(email, allowedEmails) {
		return true
	}

	// Layer 2: fall back to Slack team membership resolved from the email.
	if len(allowedTeams) > 0 && email != "" {
		if userID := emailCache.resolve(slackClient, email); userID != "" && groupCache.isMember(slackClient, userID, allowedTeams) {
			return true
		}
	}
	return false
}

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
	b.WriteString("• `/pulse create dashboard \"Acme 360\" short-name acme-360 that syncs every 10m with jira_search, salesforce_query, chorus_list_conversations`\n")
	b.WriteString("• `/ovad create a dashboard of datadog monitors in US alerting for env:prod refreshing every 5m`\n")

	b.WriteString("\n*Create a workflow* (scheduled action — can write / PR / post)\n")
	b.WriteString("• `/ovad create a workflow that every 5m polls jira open bugs in ENG with label arbetern, fixes them in github (PR assigned to claude), and posts to slack channel C0123456789`\n")
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
		b.WriteString("• `/pulse create dashboard \"Acme 360\" short-name acme-360 that syncs every 10m with jira_search, salesforce_query, chorus_list_conversations`\n")
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
		b.WriteString("• `/ovad create a workflow every 5m: poll jira open bugs in ENG with label arbetern, fix them in github (PR assigned to claude), post to slack C0123456789`\n")
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

// openVectorIndex connects the S3 Vectors index that turns the per-user
// context into a semantic memory.
func openVectorIndex(ctx context.Context, cfg *config.Config) (*vectors.Index, error) {
	embedder, err := buildEmbedder(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("embedding model: %w", err)
	}
	index, err := vectors.Open(ctx, cfg.VectorsIndexARN, embedder)
	if err != nil {
		return nil, err
	}
	log.Printf("Vector index enabled: %s (metric %s, %s @ %d dims)", index.ARN(), index.Metric(), embedder.Model(), embedder.Dimensions())
	return index, nil
}

// retryVectorIndex keeps trying to open the index in the background and
// hands it to apply once it succeeds.
func retryVectorIndex(cfg *config.Config, apply func(*vectors.Index)) {
	safego.Go("vector index: retry", func() {
		t := time.NewTicker(vectorIndexRetry)
		defer t.Stop()
		for range t.C {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			index, err := openVectorIndex(ctx, cfg)
			cancel()
			if err == nil {
				apply(index)
				return
			}
			log.Printf("Vector index still unavailable: %v", err)
		}
	})
}

// buildEmbedder picks the embeddings backend from the model name: Amazon
// Titan models go through Bedrock, anything else through the Azure OpenAI or
// GitHub Models embeddings endpoint the deployment already authenticates to.
func buildEmbedder(ctx context.Context, cfg *config.Config) (*llm.Embedder, error) {
	model := cfg.EmbeddingModel
	if model == "" {
		model = "amazon.titan-embed-text-v2:0"
	}
	dims := cfg.EmbeddingDimensions
	if dims == 0 {
		dims = llm.DefaultEmbeddingDimensions(model)
	}
	if dims == 0 {
		return nil, fmt.Errorf("EMBEDDING_DIMENSIONS is required for model %q", model)
	}
	switch {
	case strings.HasPrefix(strings.ToLower(model), "amazon."):
		region := cfg.BedrockRegion
		if region == "" {
			region = cfg.AWSRegion
		}
		if region == "" {
			region, _ = vectors.RegionOf(cfg.VectorsIndexARN)
		}
		return llm.NewBedrockEmbedder(ctx, region, model, cfg.BedrockAPIKey, dims)
	case cfg.UseAzure():
		return llm.NewAzureEmbedder(cfg.AzureEndpoint, cfg.AzureAPIKey, model, dims), nil
	case cfg.GitHubToken != "":
		return llm.NewGitHubEmbedder(cfg.GitHubToken, model, dims), nil
	}
	return nil, fmt.Errorf("no embeddings backend available for model %q", model)
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
	name := w.ShortName
	if name == "" {
		name = w.Name
	}
	return r.RunWorkflow(ctx, w.CreatedBy, w.ID, name, w.Model, prompt)
}

// dashboardPromptRenderer implements dashboards.PromptRenderer by dispatching
// to the owning agent's Router.RunDashboardPrompt (headless LLM tool loop).
type dashboardPromptRenderer struct {
	routers map[string]*commands.Router
}

func (e *dashboardPromptRenderer) RenderPrompt(ctx context.Context, agent, dashboardID, dashboardName, prompt string) (string, error) {
	r, ok := e.routers[agent]
	if !ok {
		return "", fmt.Errorf("no router for agent %q", agent)
	}
	return r.RunDashboardPrompt(ctx, "", dashboardID, dashboardName, prompt)
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
	azureClient *azure.Client,
	databricksClient *databricks.Client,
	clickhouseClient *clickhouse.Client,
	freshworksClient *freshworks.Client,
	googleClient *google.Client,
	modelsClient *llm.Client,
	codeModelsClient *llm.Client,
) {
	// --- Slack ---
	slackPerms := []permission{
		{Scope: "chat:write", Description: "Post messages and thread replies in channels", Required: true},
		{Scope: "channels:history", Description: "Read message history in public channels", Required: true},
		{Scope: "groups:history", Description: "Read message history in private channels", Required: true},
		{Scope: "im:history", Description: "Read message history in DMs", Required: true},
		{Scope: "mpim:history", Description: "Read message history in group DMs", Required: true},
		{Scope: "users:read", Description: "Read user profile information (name, email)", Required: true},
		{Scope: "users:read.email", Description: "Resolve a user's email to their Slack ID for chat-UI team RBAC", Required: true},
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

	// --- AWS (Bedrock LLM + Cost Explorer) ---
	// One cloud-provider entry covering both services, mirroring the Azure
	// entry (Azure OpenAI + Cost Management). Bedrock is the LLM backend;
	// Cost Explorer is a tool source. Either can be configured independently.
	{
		awsConnected := awsClient != nil
		bedrockConnected := cfg.UseBedrock() && modelsClient != nil

		awsPerms := []permission{}
		if bedrockConnected {
			awsPerms = append(awsPerms, permission{
				Scope: "bedrock:InvokeModel", Description: "Invoke the configured Claude model / inference profile via the Bedrock runtime", Required: true, Granted: boolPtr(true),
			})
		}
		awsPerms = append(awsPerms,
			permission{Scope: "ce:GetCostAndUsage", Description: "Query daily / monthly cost and usage aggregates with optional group-by (SERVICE, LINKED_ACCOUNT, …)", Required: true, Granted: boolPtr(awsConnected)},
			permission{Scope: "ce:GetCostForecast", Description: "Forecast upcoming cost (tomorrow through +30 days by default)", Required: true, Granted: boolPtr(awsConnected)},
			permission{Scope: "ce:GetDimensionValues", Description: "Enumerate valid dimension values (service names, accounts, usage types) for filtering", Required: false, Granted: boolPtr(awsConnected)},
		)

		active := map[string]string{}
		if bedrockConnected {
			active["Bedrock region"] = cfg.BedrockRegion
			active["General model"] = modelsClient.Model()
			if cfg.CodeModelExplicit && codeModelsClient != nil {
				active["Code model"] = codeModelsClient.Model()
			}
		}
		if awsConnected {
			active["Signing region"] = awsClient.Region()
		}

		authModes := []string{}
		if bedrockConnected {
			if cfg.BedrockAPIKey != "" {
				authModes = append(authModes, "API key (Bedrock)")
			} else {
				authModes = append(authModes, "SigV4 (AWS credential chain)")
			}
		}
		if cfg.AWSConfigured() {
			authModes = append(authModes, "SDK default credential chain (Cost Explorer)")
		}

		result = append(result, integration{
			ID:           "aws",
			Name:         "AWS",
			Configured:   awsConnected || bedrockConnected,
			AuthMode:     strings.Join(authModes, " + "),
			Permissions:  awsPerms,
			ActiveModels: active,
		})
	}

	// --- Azure (OpenAI + Cost Management) ---
	{
		openaiConnected := cfg.UseAzure() && modelsClient != nil
		costConnected := azureClient != nil
		azurePerms := []permission{}
		activeScope := map[string]string{}

		// OpenAI deployments
		if openaiConnected {
			azurePerms = append(azurePerms, permission{
				Scope: "Cognitive Services OpenAI User", Description: "Azure RBAC role for chat completions inference", Required: true, Granted: boolPtr(true),
			})

			generalModel := modelsClient.Model()
			codeModel := ""
			if codeModelsClient != nil {
				codeModel = codeModelsClient.Model()
			}
			activeScope["General model"] = generalModel
			if cfg.CodeModelExplicit {
				activeScope["Code model"] = codeModel
			}
		}

		// Cost Management
		azurePerms = append(azurePerms,
			permission{Scope: "Microsoft.CostManagement/query/action", Description: "Query daily / monthly cost aggregates with optional group-by (ServiceName, ResourceGroupName, ResourceLocation, …)", Required: true, Granted: boolPtr(costConnected)},
			permission{Scope: "Microsoft.CostManagement/forecast/action", Description: "Forecast upcoming cost (today through +30 days by default)", Required: true, Granted: boolPtr(costConnected)},
			permission{Scope: "Microsoft.CostManagement/dimensions/read", Description: "Enumerate dimension values (service names, resource groups, regions) for filtering", Required: false, Granted: boolPtr(costConnected)},
		)
		if costConnected {
			if ba := azureClient.BillingAccountID(); ba != "" {
				activeScope["Billing Account"] = ba
			} else {
				activeScope["Management Group"] = azureClient.ManagementGroupID()
			}
			activeScope["Tenant"] = azureClient.TenantID()
		}

		authModes := []string{}
		if openaiConnected {
			authModes = append(authModes, "API Key (OpenAI)")
		}
		if cfg.AzureCostConfigured() {
			authModes = append(authModes, "Service principal (Cost Management)")
		}
		authMode := strings.Join(authModes, " + ")

		result = append(result, integration{
			ID:           "azure",
			Name:         "Azure",
			Configured:   openaiConnected || costConnected,
			AuthMode:     authMode,
			Permissions:  azurePerms,
			ActiveModels: activeScope,
		})
	}

	// --- Databricks (SQL Statement Execution) ---
	{
		dbConnected := databricksClient != nil && databricksClient.Ready()
		dbPerms := []permission{
			{Scope: "sql.statement-execution", Description: "Run read-only SQL statements against a SQL warehouse (POST /api/2.0/sql/statements)", Required: true, Granted: boolPtr(dbConnected)},
			{Scope: "sql.warehouse.canuse", Description: "CAN USE on the target SQL warehouse that executes statements", Required: true, Granted: boolPtr(dbConnected)},
		}
		activeDB := map[string]string{}
		if databricksClient != nil {
			activeDB["Host"] = databricksClient.Host()
			activeDB["Warehouse"] = databricksClient.WarehouseID()
		}
		result = append(result, integration{
			ID:           "databricks",
			Name:         "Databricks",
			Configured:   cfg.DatabricksConfigured(),
			AuthMode:     "OAuth M2M (service principal)",
			Permissions:  dbPerms,
			ActiveModels: activeDB,
		})
	}

	// --- ClickHouse Cloud (Billing usage cost) ---
	{
		chConnected := clickhouseClient != nil && clickhouseClient.Ready()
		chPerms := []permission{
			{Scope: "billing.usageCost.read", Description: "Read organization usage-cost reports (GET /v1/organizations/{org}/usageCost)", Required: true, Granted: boolPtr(chConnected)},
		}
		if cfg.ClickHouseQueryConfigured() {
			chQueryConnected := clickhouseClient != nil && clickhouseClient.QueryReady()
			chPerms = append(chPerms, permission{Scope: "sql.read", Description: "Run read-only SQL against the service endpoint (SELECT / SHOW / DESCRIBE / EXISTS)", Required: false, Granted: boolPtr(chQueryConnected)})
		}
		activeCH := map[string]string{}
		if clickhouseClient != nil {
			if clickhouseClient.OrganizationID() != "" {
				activeCH["Organization"] = clickhouseClient.OrganizationID()
			}
			if clickhouseClient.QueryEndpoint() != "" {
				activeCH["Query endpoint"] = clickhouseClient.QueryEndpoint()
			}
		}
		result = append(result, integration{
			ID:           "clickhouse",
			Name:         "ClickHouse Cloud",
			Configured:   cfg.ClickHouseConfigured() || cfg.ClickHouseQueryConfigured(),
			AuthMode:     "API key (HTTP Basic)",
			Permissions:  chPerms,
			ActiveModels: activeCH,
		})
	}

	// --- Freshworks (Freshdesk / Freshchat / CRM) ---
	{
		fwPerms := []permission{
			{Scope: "freshdesk.tickets.read", Description: "Read Freshdesk tickets and conversations", Required: false, Granted: boolPtr(cfg.FreshdeskConfigured())},
			{Scope: "freshchat.conversations.read", Description: "Read Freshchat conversations and messages", Required: false, Granted: boolPtr(cfg.FreshchatConfigured())},
			{Scope: "crm.records.read", Description: "Read Freshworks CRM contacts, deals and search", Required: false, Granted: boolPtr(cfg.FreshworksCRMConfigured())},
		}
		activeFW := map[string]string{}
		if freshworksClient != nil {
			if p := freshworksClient.Products(); len(p) > 0 {
				activeFW["Products"] = strings.Join(p, ", ")
			}
		}
		result = append(result, integration{
			ID:           "freshworks",
			Name:         "Freshworks",
			Configured:   cfg.FreshworksConfigured(),
			AuthMode:     "API key / Bearer token",
			Permissions:  fwPerms,
			ActiveModels: activeFW,
		})
	}

	// --- Google Drive / Sheets ---
	{
		gConnected := googleClient != nil && googleClient.Ready()
		gPerms := []permission{
			{Scope: "drive.readonly (shared folders)", Description: "Discover and search the Drive folders shared with the service account", Required: true, Granted: boolPtr(gConnected)},
			{Scope: "drive.files.download", Description: "Stream and read file contents (CSV/text raw, Docs and Sheets exported to text)", Required: true, Granted: boolPtr(gConnected)},
			{Scope: "spreadsheets.read", Description: "Read cell ranges from a spreadsheet in a shared folder", Required: true, Granted: boolPtr(gConnected)},
			{Scope: "spreadsheets.write", Description: "Append rows to a tab of a spreadsheet in a shared folder (batched)", Required: true, Granted: boolPtr(gConnected)},
		}
		activeG := map[string]string{}
		if googleClient != nil {
			if e := googleClient.ServiceAccountEmail(); e != "" {
				activeG["Service account"] = e
			}
			if p := googleClient.ProjectID(); p != "" {
				activeG["Project"] = p
			}
			if pinned := googleClient.PinnedFolders(); len(pinned) > 0 {
				activeG["Scope"] = fmt.Sprintf("pinned to %d folder(s)", len(pinned))
			} else {
				activeG["Scope"] = "all folders shared with the service account"
			}
			// Best-effort: shows the operator what the connector actually sees
			// without making the panel refresh depend on a Drive round-trip.
			if gConnected {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if roots, rerr := googleClient.Roots(ctx); rerr == nil {
					names := make([]string, 0, len(roots))
					for _, r := range roots {
						names = append(names, r.Name)
					}
					activeG["Reachable folders"] = fmt.Sprintf("%d", len(roots))
					if len(names) > 0 && len(names) <= 8 {
						activeG["Folders"] = strings.Join(names, ", ")
					}
				}
				cancel()
			}
			if sc := googleClient.Scopes(); sc != "" {
				activeG["Scopes"] = sc
			}
		}
		result = append(result, integration{
			ID:           "google",
			Name:         "Google Drive / Sheets",
			Configured:   cfg.GoogleConfigured(),
			AuthMode:     "Service account (JWT bearer)",
			Permissions:  gPerms,
			ActiveModels: activeG,
		})
	}

	// Populate each integration's tool list for the home page "Tools" tab.
	for i := range result {
		result[i].Tools = integrationToolNames(result[i].ID)
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
	azureClient *azure.Client,
	databricksClient *databricks.Client,
	clickhouseClient *clickhouse.Client,
	freshworksClient *freshworks.Client,
	googleClient *google.Client,
	modelsClient *llm.Client,
	codeModelsClient *llm.Client,
) {
	refreshIntegrations(cfg, slackClient, ghClient, jiraClient, sfClient, chorusClient, datadogClients, awsClient, azureClient, databricksClient, clickhouseClient, freshworksClient, googleClient, modelsClient, codeModelsClient)

	safego.Go("integrations: refresh loop", func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			// Guarded per tick so one bad refresh cannot end the loop.
			safego.Run("integrations: refresh", func() {
				refreshIntegrations(cfg, slackClient, ghClient, jiraClient, sfClient, chorusClient, datadogClients, awsClient, azureClient, databricksClient, clickhouseClient, freshworksClient, googleClient, modelsClient, codeModelsClient)
			})
		}
	})
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
	switch {
	case cfg.UseBedrock():
		modelsClient, err = llm.NewBedrockClient(context.Background(), cfg.BedrockRegion, cfg.GeneralModel, cfg.BedrockAPIKey)
		if err != nil {
			log.Fatalf("Bedrock backend init failed: %v", err)
		}
		bedrockAuth := "SigV4 (AWS credential chain)"
		if cfg.BedrockAPIKey != "" {
			bedrockAuth = "API key"
		}
		log.Printf("Using AWS Bedrock backend: region %s, auth %s (general: %s)", cfg.BedrockRegion, bedrockAuth, cfg.GeneralModel)
		// The code client reuses the same credentials, signer, and connection
		// pool via WithModel — no second AWS config load or credential probe.
		codeModelsClient = modelsClient.WithModel(cfg.CodeModel)
		if cfg.CodeModelExplicit {
			log.Printf("Code model (Bedrock): %s", cfg.CodeModel)
		}
	case cfg.UseAzure():
		modelsClient = llm.NewAzureClient(cfg.AzureEndpoint, cfg.AzureAPIKey, cfg.GeneralModel)
		log.Printf("Using Azure OpenAI backend: %s (general: %s)", cfg.AzureEndpoint, cfg.GeneralModel)
		codeModelsClient = llm.NewAzureClient(cfg.AzureEndpoint, cfg.AzureAPIKey, cfg.CodeModel)
		if cfg.CodeModelExplicit {
			log.Printf("Code model (Azure): %s", cfg.CodeModel)
		}
	default:
		modelsClient = llm.NewClient(cfg.GitHubToken, cfg.GeneralModel)
		log.Printf("Using GitHub Models backend (general: %s)", cfg.GeneralModel)
		codeModelsClient = llm.NewClient(cfg.GitHubToken, cfg.CodeModel)
		if cfg.CodeModelExplicit {
			log.Printf("Code model (GitHub): %s", cfg.CodeModel)
		}
	}
	if cfg.HeadroomURL != "" {
		modelsClient.SetCompressionURL(cfg.HeadroomURL)
		codeModelsClient.SetCompressionURL(cfg.HeadroomURL)
		if cfg.HeadroomTimeout > 0 {
			modelsClient.SetCompressionTimeout(cfg.HeadroomTimeout)
			codeModelsClient.SetCompressionTimeout(cfg.HeadroomTimeout)
			log.Printf("Headroom compression enabled via %s (applies to all backends, timeout %s)", cfg.HeadroomURL, cfg.HeadroomTimeout)
		} else {
			log.Printf("Headroom compression enabled via %s (applies to all backends)", cfg.HeadroomURL)
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

	// Azure Cost Management client — OAuth client-credentials against AAD.
	// Only attempted when all four service-principal env vars are present;
	// a failed token probe in NewClient leaves the client nil so tools
	// report a clear "not configured" error.
	var azureClient *azure.Client
	if cfg.AzureCostConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		azureClient, err = azure.NewClient(ctx, cfg.AzureTenantID, cfg.AzureClientID, cfg.AzureClientSecret, cfg.AzureManagementGroupID, cfg.AzureBillingAccountID, cfg.AzureAuthorityHost, cfg.AzureManagementHost)
		cancel()
		if err != nil {
			log.Printf("Azure integration misconfigured (tools will be unavailable): %v", err)
			azureClient = nil
		} else {
			if ba := azureClient.BillingAccountID(); ba != "" {
				log.Printf("Azure integration enabled (billing account: %s, tenant: %s)", ba, azureClient.TenantID())
			} else {
				log.Printf("Azure integration enabled (management group: %s, tenant: %s)", azureClient.ManagementGroupID(), azureClient.TenantID())
			}
		}
	}

	// Databricks SQL warehouse client — OAuth 2.0 M2M (service principal)
	// against the workspace token endpoint. NewClient probes connectivity in
	// the background and retries, so the query tool becomes available once the
	// first token exchange succeeds. Only attempted when host, credentials and
	// warehouse ID are all present.
	var databricksClient *databricks.Client
	if cfg.DatabricksConfigured() {
		databricksClient = databricks.NewClient(cfg.DatabricksHost, cfg.DatabricksClientID, cfg.DatabricksClientSecret, cfg.DatabricksWarehouseID, cfg.DatabricksAllowedHostList())
		log.Printf("Databricks integration enabled (host: %s, warehouse: %s)", databricksClient.Host(), databricksClient.WarehouseID())
	}

	// ClickHouse Cloud client — the billing usage-cost API (HTTP Basic key
	// ID + secret against the Cloud API) and/or the read-only SQL query
	// interface (HTTP Basic user + password against a service's HTTPS
	// endpoint). NewClient probes each configured surface in the background and
	// retries, so each tool becomes available once its first call succeeds.
	// Built when EITHER surface is configured.
	var clickhouseClient *clickhouse.Client
	if cfg.ClickHouseConfigured() || cfg.ClickHouseQueryConfigured() {
		clickhouseClient = clickhouse.NewClient(cfg.ClickHouseKeyID, cfg.ClickHouseKeySecret, cfg.ClickHouseOrganizationID, cfg.ClickHouseQueryEndpoint, cfg.ClickHouseQueryUser, cfg.ClickHouseQueryPassword)
		if cfg.ClickHouseConfigured() {
			log.Printf("ClickHouse integration enabled (organization: %s)", clickhouseClient.OrganizationID())
		}
		if cfg.ClickHouseQueryConfigured() {
			log.Printf("ClickHouse SQL query interface enabled (endpoint: %s)", clickhouseClient.QueryEndpoint())
		}
	}

	// Freshworks suite (read-only) — Freshdesk (tickets), Freshchat
	// (conversations) and Freshworks CRM (sales). Each product is configured
	// independently; the umbrella client leaves a product's sub-client nil when
	// its credentials are absent, and the command layer only advertises the
	// tools whose product is configured.
	var freshworksClient *freshworks.Client
	if cfg.FreshworksConfigured() {
		freshworksClient = freshworks.NewClient(cfg.FreshdeskDomain, cfg.FreshdeskAPIKey, cfg.FreshchatURL, cfg.FreshchatAPIToken, cfg.FreshworksCRMDomain, cfg.FreshworksCRMAPIKey)
		log.Printf("Freshworks integration enabled (products: %v)", freshworksClient.Products())
	}

	// Google Drive / Sheets client — a service account authenticating with the
	// JWT-bearer grant. Scope is the Drive share itself: the client discovers
	// every folder and shared drive that has been shared with the account and
	// works across all of them, so granting access is a Drive share with no
	// redeploy. google-drive-folder-ids optionally confines it to specific
	// folders instead.
	//
	// NewClient fails only on an unusable key (a deployment mistake worth logging
	// loudly); a failed first token exchange leaves the client disconnected and
	// retrying in the background, so the tools become available once it connects.
	// Reachable folders are resolved once at boot so a deployment where nothing
	// was shared with the service account says so in the logs rather than
	// surfacing as a puzzling empty search on the first workflow tick.
	var googleClient *google.Client
	if cfg.GoogleConfigured() {
		googleClient, err = google.NewClient(cfg.GoogleCredentialsJSON, cfg.GoogleDriveFolderIDs, cfg.GoogleScopes)
		if err != nil {
			log.Printf("Google integration misconfigured (tools will be unavailable): %v", err)
			googleClient = nil
		} else {
			scope := "all folders shared with the account"
			if pinned := googleClient.PinnedFolders(); len(pinned) > 0 {
				scope = "pinned to " + strings.Join(pinned, ", ")
			}
			log.Printf("Google integration enabled (service account: %s, scope: %s)", googleClient.ServiceAccountEmail(), scope)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			roots, verr := googleClient.VerifyAccess(ctx)
			cancel()
			switch {
			case verr != nil:
				log.Printf("Google Drive access check failed — %v", verr)
			case len(roots) == 1:
				log.Printf("Google Drive: 1 folder reachable (%q, %s) — it will be used automatically", roots[0].Name, roots[0].ID)
			default:
				names := make([]string, 0, len(roots))
				for _, r := range roots {
					names = append(names, fmt.Sprintf("%q", r.Name))
				}
				log.Printf("Google Drive: %d locations reachable: %s", len(roots), strings.Join(names, ", "))
			}
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
	startIntegrationsRefresher(cfg, slackClient, ghClient, jiraClient, sfClient, chorusClient, datadogClients, awsClient, azureClient, databricksClient, clickhouseClient, freshworksClient, googleClient, modelsClient, codeModelsClient)

	// State backend: every registry below reads and writes the S3 bucket and
	// keeps only a cache in memory, so any replica can serve any request and a
	// restart loses nothing.
	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelBoot()
	backend, err := store.Open(bootCtx, cfg.StateBackendARN)
	if err != nil {
		log.Fatalf("state backend: %v", err)
	}
	log.Printf("State backend: %s (region %s, instance %s)", backend, backend.Region(), store.InstanceID())

	// Deferred work: anything a requester should not wait for is handed to the
	// queue, which keeps its tasks in the same bucket and claims them with
	// conditional writes. Every replica runs a worker — the claim is the lock,
	// so no leader is involved and the backlog spreads across the fleet.
	tasks := queue.New(backend)

	// Thread sessions live in the bucket so a reply in a thread may be
	// answered by any replica.
	sessions := commands.NewSessionStore(backend, cfg.ThreadSessionTTL)
	sessions.SetSlack(slackClient)
	log.Printf("Thread sessions: %ssessions/ (TTL %s)", backend, cfg.ThreadSessionTTL)
	var vectorIndex *vectors.Index
	var vectorErr error
	if cfg.VectorsIndexARN != "" {
		vectorIndex, vectorErr = openVectorIndex(bootCtx, cfg)
	}

	// Per-user context store. Populated after every Slack request (DMs,
	// channels, and in-thread follow-ups all flow through the same
	// handlers) and read back into the system prompt on every subsequent
	// request so the agent can recognise recurring user topics. With a
	// vector index attached, the turns shown are the ones relevant to the
	// current question rather than simply the latest.
	userContextStore := commands.NewUserContextStore(backend, vectorIndex)
	sharedAgents := make([]string, 0, len(agents))
	for _, agent := range agents {
		if agent.SharedMemory {
			sharedAgents = append(sharedAgents, agent.ID)
		}
	}
	userContextStore.SetSharedAgents(sharedAgents)
	userContextStore.SetIdentityLinker(newIdentityLinks(slackClient, 24*time.Hour).resolve)
	userContextStore.UseQueue(tasks)
	log.Printf("User-context store: %suser-context/ (TTL=%s, semantic=%t, shared agents=%d)", backend, commands.UserContextTTL, userContextStore.Semantic(), len(sharedAgents))

	// RBAC: build agentID → allowedTeams map and group membership cache.
	agentRBAC := make(map[string][]string, len(agents))
	for _, agent := range agents {
		agentRBAC[agent.ID] = agent.AllowedTeams
		if len(agent.AllowedTeams) > 0 {
			log.Printf("RBAC: agent %q restricted to teams %v", agent.ID, agent.AllowedTeams)
		}
	}
	rbacCache := newGroupMemberCache(5 * time.Minute)
	// Cache for resolving OAuth-proxy emails → Slack user IDs, used by the
	// chat-UI authorizer to fall back to allowed_teams membership.
	emailUserCache := newEmailUserIDCache(5 * time.Minute)

	// Map of agentID → Router so the events handler can dispatch thread replies.
	routers := make(map[string]*commands.Router, len(agents))

	// Dashboards registry: background sync of LLM-created data dashboards.
	// Boot only loads state; the sync tickers start once this replica holds
	// the scheduling lease (see the election below), and never fire an
	// immediate sync — that would hammer every upstream on startup.
	dashExec := dashboards.NewExecutor(dashboards.Clients{
		Jira:    jiraClient,
		SF:      sfClient,
		Chorus:  chorusClient,
		Datadog: datadogClients,
		GitHub:  ghClient,
	})
	dashRegistry := dashboards.New(backend, dashExec)
	if err := dashRegistry.LoadAll(bootCtx); err != nil {
		log.Fatalf("failed to load dashboards: %v", err)
	}
	defer dashRegistry.StopAll()

	// Workflows registry: scheduled LLM tool-loop runs. The executor is wired
	// below once the routers map is populated.
	wfRegistry := workflows.New(backend, nil)
	wfRegistry.UseQueue(tasks)
	if err := wfRegistry.LoadAll(bootCtx); err != nil {
		log.Fatalf("failed to load workflows: %v", err)
	}
	defer wfRegistry.StopAll()

	// Catalog: workflows and dashboards searchable by meaning through the
	// same vector index as the user context.
	// The state bucket holds transcripts, sessions and connector settings, so
	// its view is closed unless an allow-list admits the user.
	canViewBackend := func(r *http.Request) bool {
		if len(cfg.BackendViewTeams) == 0 && len(cfg.BackendViewEmails) == 0 {
			return false
		}
		return uiRBACAllowed(r, cfg.BackendViewEmails, cfg.BackendViewTeams, slackClient, emailUserCache, rbacCache)
	}
	backendUI := newBackendView(backend, canViewBackend)
	backendUI.setIndex(vectorIndex)
	catalogIndex := catalog.New(backend, wfRegistry, dashRegistry)
	catalogIndex.SetIndex(vectorIndex)
	if vectorErr != nil {
		log.Printf("Vector index disabled, retrying every %s: %v", vectorIndexRetry, vectorErr)
		retryVectorIndex(cfg, func(index *vectors.Index) {
			userContextStore.SetIndex(index)
			catalogIndex.SetIndex(index)
			backendUI.setIndex(index)
		})
	}

	// Usage & Billing store: aggregates LLM token cost of every Slack,
	// workflow, and chat turn and merges it into the bucket periodically.
	billingStore, err := billing.New(bootCtx, backend)
	if err != nil {
		log.Fatalf("failed to init billing store: %v", err)
	}
	log.Printf("Usage & billing store: %sbilling/ (%d month(s))", backend, billingStore.Months())
	billingStop := make(chan struct{})
	billingDone := billingStore.StartFlusher(billingStop)
	userNames := newUserNameCache(24 * time.Hour)
	billingStore.SetUserNameResolver(func(id string) string { return userNames.resolve(slackClient, id) })
	billing.StartPriceSync(billingStop)

	// Performance series: how long turns, model calls and tool calls take, and
	// how often they fail. Kept apart from the usage ledger on purpose — it
	// records no requester, channel or workflow owner, only the agent, entry
	// path, model and tool.
	perfStore, err := metrics.New(bootCtx, backend)
	if err != nil {
		log.Fatalf("failed to init metrics store: %v", err)
	}
	log.Printf("Performance store: %smetrics/ (%d month(s))", backend, perfStore.Months())
	perfStop := make(chan struct{})
	perfDone := perfStore.StartFlusher(perfStop)
	perfStore.SetQueueStats(func() any { return tasks.Stats() })
	perfStore.SetDependencies(func() any { return llm.Dependencies() })
	llm.SetObserver(llm.ObserverFunc(func(c llm.CallStats) {
		perfStore.RecordCall(metrics.Call{
			Model:       c.Model,
			Backend:     c.Backend,
			LatencyMS:   c.Latency.Milliseconds(),
			Attempts:    c.Attempts,
			RateLimited: c.RateLimited,
			Failed:      c.Failed,
		})
	}))

	agentIDs := make([]string, 0, len(agents))
	for _, a := range agents {
		agentIDs = append(agentIDs, a.ID)
	}
	skillRegistry, err := skills.New(bootCtx, backend)
	if err != nil {
		log.Fatalf("failed to load skills: %v", err)
	}
	skillRegistry.SetKnownAgents(agentIDs)
	skillRegistry.SetBuiltin(func() []skills.Skill { return builtinSkills(agents) })
	log.Printf("Skills store: %sskills/ (%d custom skill(s))", backend, skillRegistry.Count())
	mcpRegistry, err := mcp.New(bootCtx, backend)
	if err != nil {
		log.Fatalf("failed to load MCP connectors: %v", err)
	}
	mcpRegistry.SetKnownAgents(agentIDs)
	log.Printf("MCP connectors store: %smcp/ (%d connector(s))", backend, mcpRegistry.Count())
	// Who may add, change, test or delete connectors. Reads stay open; the
	// identity endpoint tells the UI whether to offer the actions at all.
	canManageMCP := func(r *http.Request) bool {
		return uiRBACAllowed(r, cfg.MCPAdminEmails, cfg.MCPAdminTeams, slackClient, emailUserCache, rbacCache)
	}
	mcp.SetAllowedEnv(cfg.MCPAllowedEnv)
	if len(cfg.MCPAllowedEnv) > 0 {
		log.Printf("MCP connectors may expand MCP_* and %v", cfg.MCPAllowedEnv)
	}
	if len(cfg.MCPAdminTeams) > 0 || len(cfg.MCPAdminEmails) > 0 {
		mcpRegistry.SetAuthorizer(func(r *http.Request) bool {
			return checkChatRBAC(r, "mcp-connectors", cfg.MCPAdminEmails, cfg.MCPAdminTeams, slackClient, emailUserCache, rbacCache)
		})
		log.Printf("RBAC: MCP connector changes restricted to emails %v / teams %v", cfg.MCPAdminEmails, cfg.MCPAdminTeams)
	}

	for _, agent := range agents {
		ap, err := prompts.LoadAgent(agent.ID)
		if err != nil {
			log.Fatalf("failed to load prompts for agent %s: %v", agent.ID, err)
		}
		ap.SetSkillProvider(skillRegistry)

		agentID := agent.ID // capture for closure

		// Per-agent credential overrides: when the Helm chart mounts an
		// arbetern-<agent>-secrets Secret at $AGENT_CREDENTIALS_DIR/<agent>/,
		// rebuild only the integration clients whose credentials actually
		// differ from the global config. Everything else falls through to
		// the shared client (no extra connections, no extra goroutines).
		agentCfg := cfg.ForAgent(agentID)
		agentClients := buildAgentScopedClients(cfg, agentCfg, agentID, agentIntegrationClients{
			jira:       jiraClient,
			sf:         sfClient,
			chorus:     chorusClient,
			datadog:    datadogClients,
			aws:        awsClient,
			azure:      azureClient,
			nvd:        nvdClient,
			databricks: databricksClient,
			clickhouse: clickhouseClient,
			freshworks: freshworksClient,
			google:     googleClient,
		})

		router := commands.NewRouter(slackClient, ghClient, modelsClient, codeModelsClient, agentClients.jira, agentClients.nvd, agentClients.sf, agentClients.chorus, agentClients.datadog, agentClients.aws, agentClients.azure, agentClients.databricks, agentClients.clickhouse, agentClients.freshworks, agentClients.google, dashRegistry, wfRegistry, ap, agent.ID, cfg.AppURL, sessions, cfg.MaxToolRounds, userContextStore, billingStore)
		router.SetMCP(mcpRegistry)
		router.SetCatalog(catalogIndex)
		router.SetPerf(perfStore)
		routers[agent.ID] = router

		// Sweeps the per-router channel-history cache so inactive channels
		// do not accumulate.
		router.ContextProvider().StartGC(context.Background(), router.ContextProvider().TTL())

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

	setToolDescriptions(routers)

	// Centralized per-agent chat (UI-driven). Disabled per agent by default;
	// enabled via `chat_enabled: true` in the agent's config.yaml. There is no
	// user auth yet, so each agent's conversations are shared by every viewer.
	// The responder replays recent history and runs the agent's full tool
	// loop (RunChat) so the chat can use the same integrations as a Slack
	// command.
	sessions.SetRouterResolver(func(agentID string) *commands.Router { return routers[agentID] })

	chatRegistry := chat.New(backend, func(ctx context.Context, agentID, user string, history []chat.Message, userMessage string, tracker *progress.Tracker) (string, error) {
		router := routers[agentID]
		if router == nil {
			return "", fmt.Errorf("no router configured for agent %q", agentID)
		}
		msgs := make([]llm.ChatMessage, 0, len(history))
		for _, m := range history {
			role := m.Role
			if role != "user" && role != "assistant" {
				role = "user"
			}
			msgs = append(msgs, llm.NewChatMessage(role, m.Content))
		}
		// RunChat gives the UI chat the same tool access (GitHub, Jira,
		// Datadog, Databricks, …) and multi-round agentic loop as a Slack
		// command — replacing the old single-shot, tool-less completion that
		// could only role-play "running the query" without executing anything.
		// user is the OAuth-proxy-verified email (when present) used to
		// attribute a created Jira ticket's reporter to the requester.
		return router.RunChat(ctx, user, msgs, userMessage, tracker)
	})
	if err := chatRegistry.Load(bootCtx); err != nil {
		log.Fatalf("failed to load chat transcripts: %v", err)
	}
	log.Printf("Chat store: %schat/ (%d conversation(s))", backend, chatRegistry.Count())
	for _, agent := range agents {
		chatRegistry.SetEnabled(agent.ID, agent.ChatEnabled)
		if agent.ChatEnabled {
			log.Printf("Chat enabled for agent %q", agent.ID)
		}
	}

	log.Printf("Chat retention: conversations inactive for >%s are auto-deleted", cfg.ChatRetention)

	// Per-agent UI/chat RBAC. allowed_emails (OAuth-proxy-verified email) is the
	// primary layer; allowed_teams (Slack user-group membership, resolved from
	// the email) is the fallback. Empty for both = no restriction. Used by the
	// chat API authorizer below and the /ui/<agent>/chat deep-link route.
	agentEmailRBAC := make(map[string][]string, len(agents))
	for _, agent := range agents {
		agentEmailRBAC[agent.ID] = agent.AllowedEmails
		if len(agent.AllowedEmails) > 0 {
			log.Printf("RBAC: agent %q chat restricted to emails %v", agent.ID, agent.AllowedEmails)
		}
		if len(agentRBAC[agent.ID]) > 0 {
			log.Printf("RBAC: agent %q chat falls back to Slack teams %v", agent.ID, agentRBAC[agent.ID])
		}
	}
	chatRegistry.SetAuthorizer(func(req *http.Request, agentID string) bool {
		return checkChatRBAC(req, agentID, agentEmailRBAC[agentID], agentRBAC[agentID], slackClient, emailUserCache, rbacCache)
	})
	// Skills follow the same allow-lists: a skill may only be added, changed
	// or deleted by someone allowed to use every agent it targets.
	canManageSkillsFor := func(r *http.Request, agentID string) bool {
		return uiRBACAllowed(r, agentEmailRBAC[agentID], agentRBAC[agentID], slackClient, emailUserCache, rbacCache)
	}
	skillAgentsFor := func(r *http.Request) []string {
		out := make([]string, 0, len(agentIDs))
		for _, id := range agentIDs {
			if canManageSkillsFor(r, id) {
				out = append(out, id)
			}
		}
		return out
	}
	skillRegistry.SetAuthorizer(func(r *http.Request, scope []string) bool {
		targets := scope
		if len(targets) == 0 {
			targets = agentIDs
		}
		for _, id := range targets {
			if !canManageSkillsFor(r, id) {
				log.Printf("[rbac] DENIED email=%q scope=skills/%s (allowed_emails=%v allowed_teams=%v)", redactEmail(clientEmail(r)), id, agentEmailRBAC[id], agentRBAC[id])
				return false
			}
		}
		return true
	})

	// Editing or running a workflow or dashboard drives the owning agent's
	// tools, so it takes the same permission as using that agent.
	canManageAgent := func(r *http.Request, agentID string) bool {
		if canManageSkillsFor(r, agentID) {
			return true
		}
		log.Printf("[rbac] DENIED email=%q scope=agent/%s (allowed_emails=%v allowed_teams=%v)",
			redactEmail(clientEmail(r)), agentID, agentEmailRBAC[agentID], agentRBAC[agentID])
		return false
	}
	// A GitOps reconcile touches every agent's descriptors, so it takes
	// permission on all of them.
	canManageAllAgents := func(r *http.Request) bool {
		for _, id := range agentIDs {
			if !canManageSkillsFor(r, id) {
				return false
			}
		}
		return true
	}

	// Attribute chat messages to the OAuth-proxy-verified sender. When a proxy is
	// in front, clientEmail returns the authenticated email; with no proxy (local
	// dev) it returns "" and the UI shows "Anonymous".
	chatRegistry.SetUserResolver(clientEmail)

	knownAgents := make(map[string]bool, len(agents))
	for _, a := range agents {
		knownAgents[a.ID] = true
	}

	// Deep-link route for the full-screen chat: /ui/<agent>/chat. Serves the
	// SPA shell (the front-end router opens the agent's chat from the path).
	// Access is enforced by the chat API authorizer, not here: unauthorized
	// users still load the shell but get a friendly "no access" message from
	// the front-end instead of a raw 403 page. Only the agent's chat data is
	// gated, and the same shell is already public at /ui/.
	// More specific than the "/ui/" static handler, so it takes precedence.
	uiContent, _ := fs.Sub(uiFS, "ui")
	uiStatic := http.StripPrefix("/ui/", http.FileServer(http.FS(uiContent)))
	if indexHTML, err := uiFS.ReadFile("ui/index.html"); err == nil {
		serveShell := func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(indexHTML)
		}
		http.HandleFunc("/ui/{agent}/chat", func(w http.ResponseWriter, r *http.Request) {
			agent := r.PathValue("agent")
			if !chatRegistry.IsEnabled(agent) {
				http.NotFound(w, r)
				return
			}
			serveShell(w)
		})
		// Detail pages of one workflow or dashboard: the shell renders them
		// from /api/<kind>s/<agent>/<id>, which answers 404 for unknown ids.
		detailPage := func(w http.ResponseWriter, r *http.Request) {
			if !knownAgents[r.PathValue("agent")] || !store.IDRe.MatchString(r.PathValue("id")) {
				http.NotFound(w, r)
				return
			}
			serveShell(w)
		}
		http.HandleFunc("/ui/{agent}/workflow/{id}", detailPage)
		http.HandleFunc("/ui/{agent}/dashboard/{id}", detailPage)
		// Client-routed pages of the management UI share the SPA shell; any
		// other single-segment path under /ui/ is a static asset.
		http.HandleFunc("/ui/{page}", func(w http.ResponseWriter, r *http.Request) {
			if uiPages[r.PathValue("page")] {
				serveShell(w)
				return
			}
			uiStatic.ServeHTTP(w, r)
		})
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

				// A thread reply drives the same tools as the command that
				// opened the thread, so it takes the same team membership.
				// Without this, any workspace member could steer a restricted
				// agent by replying in someone else's thread.
				if !checkAgentRBAC(rbacCache, slackClient, sess.AgentID, userID, agentRBAC[sess.AgentID]) {
					log.Printf("[session] thread reply denied by RBAC: channel=%s thread=%s user=%s agent=%s",
						channelID, threadTS, userID, sess.AgentID)
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
		safego.Go("slack: socket listener", socketListener.Start)
		log.Printf("Socket Mode enabled — listening for thread replies")
	} else {
		log.Printf("Warning: SLACK_APP_TOKEN not set — thread session follow-ups disabled")
	}

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Agent management UI (embedded static files) — behind IP whitelist if configured.
	trusted := parseCIDRs(cfg.TrustedProxyCIDRs)
	setTrustedProxies(trusted)
	identityHeadersOpen = len(trusted) == 0
	if identityHeadersOpen {
		guarded := len(cfg.MCPAdminTeams) > 0 || len(cfg.MCPAdminEmails) > 0 ||
			len(cfg.BackendViewTeams) > 0 || len(cfg.BackendViewEmails) > 0
		for _, a := range agents {
			guarded = guarded || len(a.AllowedEmails) > 0 || len(a.AllowedTeams) > 0
		}
		if guarded {
			log.Printf("WARNING: TRUSTED_PROXY_CIDRS is not set, so X-Auth-Request-Email is believed from any peer. " +
				"Anything able to reach this pod can claim any identity and pass every allow-list. " +
				"Set TRUSTED_PROXY_CIDRS to the address range your auth proxy connects from.")
		}
		if strings.TrimSpace(cfg.UIAllowedCIDRs) != "" {
			log.Printf("WARNING: TRUSTED_PROXY_CIDRS is not set, so the UI_ALLOWED_CIDRS gate reads the first " +
				"X-Forwarded-For entry, which a client can set. Set TRUSTED_PROXY_CIDRS to your load balancer's " +
				"address range to take the real client from the rightmost untrusted hop instead.")
		}
	} else {
		log.Printf("Identity headers trusted only from %s", cfg.TrustedProxyCIDRs)
	}

	uiCIDRs := parseCIDRs(cfg.UIAllowedCIDRs)
	if len(uiCIDRs) > 0 {
		log.Printf("UI IP whitelist enabled: %s", cfg.UIAllowedCIDRs)
	}
	// Per-route IP gating is no longer needed — globalIPGate (installed on
	// the server Handler below) covers every non-exempt path in one place.
	http.Handle("/ui/", uiStatic)
	// Favicon — exempt from the IP whitelist so it loads from anywhere.
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

	// API: list agents with their prompts (read-only, discovered from agents/
	// directory). The result is cached in memory with a short TTL and shared by
	// all viewers, so repeated landing-page loads re-render from the cache
	// instead of re-walking the agents/ directory on every request.
	apiMux := http.NewServeMux()
	agentList := &agentsCache{}
	agentList.seed(agents)
	apiMux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		agents, err := agentList.get()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to discover agents: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agents)
	})

	identities := newIdentityCache(10 * time.Minute)
	apiMux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(struct {
			identity
			MCPAdmin     bool     `json:"mcp_admin"`
			BackendAdmin bool     `json:"backend_admin"`
			SkillAgents  []string `json:"skill_agents"`
		}{identities.lookup(clientEmail(r), slackClient, jiraClient, cfg.AtlassianURL), canManageMCP(r), canViewBackend(r), skillAgentsFor(r)})
	})

	// API: the signed-in person's own stored context, aggregated across every
	// agent. This is not the backend view under a different name: it reads only
	// the documents keyed to the identities this request authenticated as, so
	// it needs no allow-list to show someone their own memory.
	apiMux.HandleFunc("/api/me/context", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		me := identities.lookup(clientEmail(r), slackClient, jiraClient, cfg.AtlassianURL)
		if me.Anonymous {
			httpx.WriteJSON(w, http.StatusOK, map[string]bool{"anonymous": true})
			return
		}
		ids := []string{me.Email}
		if me.Slack != nil {
			ids = append(ids, me.Slack.ID)
		}
		profile, err := userContextStore.Profile(r.Context(), ids)
		if err != nil {
			log.Printf("[user-context] profile for %s failed: %v", redactEmail(me.Email), err)
			http.Error(w, "failed to read your stored context", http.StatusBadGateway)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, profile)
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
		_ = json.NewEncoder(w).Encode(describeIntegrations(data))
	})

	// API: thread session stats (observability).
	apiMux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		active, opened, expired, explicit := sessions.Stats(r.Context())
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
	changelog := newTTLCache(changelogCacheTTL, func(ctx context.Context) ([]github.CommitSummary, error) {
		commits, err := ghClient.ListCommits(ctx, changelogOwner, changelogRepo, "", "", "", time.Time{}, time.Time{}, 20)
		if err != nil {
			return nil, err
		}
		for i := range commits {
			if len(commits[i].SHA) > 7 {
				commits[i].SHA = commits[i].SHA[:7]
			}
		}
		return commits, nil
	})
	apiMux.HandleFunc("/api/changes", func(w http.ResponseWriter, r *http.Request) {
		if ghClient == nil {
			http.Error(w, "GitHub integration not configured", http.StatusServiceUnavailable)
			return
		}
		commits, err := changelog.get(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to fetch commits: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(commits)
	})

	// API: open pull requests the agents authored, found by the body marker.
	pulls := newTTLCache(pullsCacheTTL, func(ctx context.Context) ([]github.AutomatedPR, error) {
		prs, err := ghClient.ListOpenAutomatedPullRequests(ctx)
		if err != nil {
			return nil, err
		}
		for i := range prs {
			if prs[i].Agent == "" {
				prs[i].Agent = agentFromTitle(prs[i].Title, knownAgents)
			}
		}
		return prs, nil
	})
	apiMux.HandleFunc("/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		if ghClient == nil {
			http.Error(w, "GitHub integration not configured", http.StatusServiceUnavailable)
			return
		}
		prs, err := pulls.get(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to fetch pull requests: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(prs)
	})

	// API: unresolved Jira issues assigned to the integration's own account.
	tickets := newTTLCache(ticketsCacheTTL, func(context.Context) ([]atlassian.IssueSummary, error) {
		return jiraClient.AssignedIssues(200)
	})
	jiraReady := func() bool { return jiraClient != nil && jiraClient.Ready() }
	apiMux.HandleFunc("/api/tickets", func(w http.ResponseWriter, r *http.Request) {
		if !jiraReady() {
			http.Error(w, "Jira integration not configured", http.StatusServiceUnavailable)
			return
		}
		issues, err := tickets.get(r.Context())
		if err != nil {
			http.Error(w, "failed to fetch tickets", http.StatusInternalServerError)
			return
		}
		if issues == nil {
			issues = []atlassian.IssueSummary{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issues)
	})
	if jiraReady() {
		safego.Go("warm: tickets", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := tickets.get(ctx); err != nil {
				log.Printf("warn: tickets warm-up failed: %v", err)
			}
		})
	}

	backendUI.mount(apiMux)

	// Fill the caches the console reads on load that are not already warm:
	// the commit list and open pull requests (GitHub calls) and the usage
	// summary, whose first pass starts resolving Slack display names for the
	// user leaderboards.
	if ghClient != nil {
		safego.Go("warm: changelog", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := changelog.get(ctx); err != nil {
				log.Printf("warn: changelog warm-up failed: %v", err)
			}
		})
		safego.Go("warm: pull requests", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := pulls.get(ctx); err != nil {
				log.Printf("warn: pull requests warm-up failed: %v", err)
			}
		})
	}
	safego.Go("warm: usage summary", func() { _ = billingStore.Summarize(30) })
	safego.Go("warm: performance summary", func() { _ = perfStore.Summarize(30) })

	http.Handle("/api/", apiMux)

	// Per-agent data routes (/<agent>/<kind>/<id>/data.json, with the bare
	// path redirecting into the console) and the /api/<kind>s API.
	dashRegistry.RegisterRoutes(http.DefaultServeMux, apiMux, knownAgents, canManageAgent)
	wfRegistry.RegisterRoutes(http.DefaultServeMux, apiMux, knownAgents, canManageAgent)
	chatRegistry.RegisterRoutes(apiMux, knownAgents)
	billingStore.RegisterRoutes(http.DefaultServeMux, apiMux)
	perfStore.RegisterRoutes(apiMux)
	skillRegistry.RegisterRoutes(apiMux, clientEmail)
	mcpRegistry.RegisterRoutes(apiMux, clientEmail)

	// Wire the workflow executor now that routers are built. Tick goroutines
	// start when this replica acquires the scheduling lease below.
	wfRegistry.SetExecutor(&workflowExecutor{routers: routers})

	// Wire the dashboard prompt renderer now that routers are built. Prompt
	// dashboards (and their per-input instances) render through the owning
	// agent's LLM tool-loop; their sync tickers were started at boot but
	// no-op until this is set.
	dashRegistry.SetPromptRenderer(&dashboardPromptRenderer{routers: routers})

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
				s.OnStatus(persistGitOpsStatus(backend, "workflows"))
				wfSyncer = s
			}
		}
	}
	registerGitOpsRoutes(apiMux, "workflows", canManageAllAgents, sharedGitOpsStatus(backend, "workflows", func() any {
		if wfSyncer == nil {
			return nil
		}
		return wfSyncer.Status()
	}), func(ctx context.Context) error {
		if wfSyncer == nil {
			return errGitOpsDisabled
		}
		return wfSyncer.SyncNow(ctx)
	})

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
				s.OnStatus(persistGitOpsStatus(backend, "dashboards"))
				dashSyncer = s
			}
		}
	}
	registerGitOpsRoutes(apiMux, "dashboards", canManageAllAgents, sharedGitOpsStatus(backend, "dashboards", func() any {
		if dashSyncer == nil {
			return nil
		}
		return dashSyncer.Status()
	}), func(ctx context.Context) error {
		if dashSyncer == nil {
			return errGitOpsDisabled
		}
		return dashSyncer.SyncNow(ctx)
	})

	// Search by meaning over the catalogued descriptors; exact paths, so they
	// win over the crud prefix handlers.
	for _, kind := range []string{catalog.KindWorkflow, catalog.KindDashboard} {
		kind := kind
		apiMux.HandleFunc("/api/"+kind+"s/_search", func(w http.ResponseWriter, r *http.Request) {
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			if q == "" {
				http.Error(w, "q is required", http.StatusBadRequest)
				return
			}
			hits, err := catalogIndex.Search(r.Context(), kind, r.URL.Query().Get("agent"), q, catalog.MaxResults)
			switch {
			case errors.Is(err, catalog.ErrDisabled):
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			case err != nil:
				log.Printf("[catalog] %s search failed: %v", kind, err)
				http.Error(w, "search failed", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(map[string]any{"query": q, "kind": kind, "hits": hits})
		})
	}

	// Deferred work runs on every replica, not only the scheduling leader: a
	// task is claimed with a conditional write, so the backlog is shared out
	// rather than piled onto one pod.
	queueCtx, cancelQueue := context.WithCancel(context.Background())
	defer cancelQueue()
	tasks.Start(queueCtx)
	log.Printf("Deferred work: %squeue/ (topics %v)", backend, tasks.Topics())

	// Every replica keeps its caches in step with the bucket so UI reads and
	// tool calls see what other replicas wrote.
	for _, start := range []func(context.Context, time.Duration){
		wfRegistry.StartRefresh, dashRegistry.StartRefresh, chatRegistry.StartRefresh,
		skillRegistry.StartRefresh, mcpRegistry.StartRefresh,
		billingStore.StartRefresh, perfStore.StartRefresh,
	} {
		start(context.Background(), stateRefreshInterval)
	}

	// Scheduling lease: one replica at a time runs workflow tickers, dashboard
	// syncs, GitOps reconciles and retention sweeps. Every replica serves
	// Slack, chat and the UI; its writes land in the bucket and the leader
	// picks them up on its next refresh.
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	safego.Go("scheduling lease", func() {
		defer close(leaderDone)
		lease := store.NewLease(backend, "locks/scheduler", store.InstanceID(), schedulerLeaseTTL)
		store.RunElection(leaderCtx, lease, func(held context.Context) {
			log.Printf("[lease] %s is scheduling", store.InstanceID())
			wfRegistry.StartAllEnabled(held)
			dashRegistry.StartAll(held)
			if wfSyncer != nil {
				wfSyncer.Start(held)
			}
			if dashSyncer != nil {
				dashSyncer.Start(held)
			}
			chatRegistry.StartRetention(held, cfg.ChatRetention, time.Hour)
			userContextStore.StartGC(held, time.Hour)
			sessions.StartSweeper(held, time.Minute)
			catalogIndex.StartSync(held, catalogSyncInterval)
			if n, err := dashRegistry.PurgeLegacyCache(held); err != nil {
				log.Printf("[dashboards] legacy cache purge: %v", err)
			} else if n > 0 {
				log.Printf("[dashboards] removed %d legacy cache object(s)", n)
			}
			<-held.Done()
			wfRegistry.StopAll()
			dashRegistry.StopAll()
			log.Printf("[lease] %s stopped scheduling", store.InstanceID())
		})
	})

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
		Addr:              ":" + cfg.Port,
		Handler:           gatedHandler,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      6 * time.Minute,
		IdleTimeout:       120 * time.Second,
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
	// Hand the scheduling lease over right away rather than letting it expire,
	// then merge the last buffered usage into the bucket.
	cancelLeader()
	cancelQueue()
	select {
	case <-leaderDone:
	case <-time.After(10 * time.Second):
	}
	// Both final flushes start as soon as their stop channel closes, so they
	// share one deadline rather than queueing two.
	close(billingStop)
	close(perfStop)
	flushed := time.After(30 * time.Second)
	for _, done := range []<-chan struct{}{billingDone, perfDone} {
		select {
		case <-done:
		case <-flushed:
		}
	}
	log.Println("server stopped")
}
