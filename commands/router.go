package commands

import (
	"context"
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/aws"
	"github.com/justmike1/arbetern/azure"
	"github.com/justmike1/arbetern/billing"
	"github.com/justmike1/arbetern/catalog"
	"github.com/justmike1/arbetern/chorus"
	"github.com/justmike1/arbetern/clickhouse"
	"github.com/justmike1/arbetern/dashboards"
	"github.com/justmike1/arbetern/databricks"
	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/document360"
	"github.com/justmike1/arbetern/freshworks"
	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/google"
	"github.com/justmike1/arbetern/internal/journal"
	"github.com/justmike1/arbetern/internal/progress"
	"github.com/justmike1/arbetern/llm"
	"github.com/justmike1/arbetern/mcp"
	"github.com/justmike1/arbetern/nvd"
	"github.com/justmike1/arbetern/salesforce"
	"github.com/justmike1/arbetern/slack"
	"github.com/justmike1/arbetern/workflows"
)

type Router struct {
	slackClient       SlackClient
	ghClient          *github.Client
	modelsClient      *llm.Client
	codeModelsClient  *llm.Client
	jiraClient        *atlassian.Client
	nvdClient         *nvd.Client
	sfClient          *salesforce.Client
	chorusClient      *chorus.Client
	datadogClients    *datadog.MultiClient
	awsClient         *aws.Client
	azureClient       *azure.Client
	databricksClient  *databricks.Client
	clickhouseClient  *clickhouse.Client
	freshworksClient  *freshworks.Client
	document360Client *document360.Client
	googleClient      *google.Client
	dashboards        *dashboards.Registry
	workflows         *workflows.Registry
	mcp               *mcp.Registry
	contextProvider   *ContextProvider
	catalog           *catalog.Index
	prompts           PromptProvider
	agentID           string
	appURL            string
	sessions          *SessionStore
	maxToolRounds     int
	userContextStore  *UserContextStore
	billing           UsageRecorder
	perf              PerfRecorder
	inflight          *journal.Journal
}

func NewRouter(slackClient SlackClient, ghClient *github.Client, modelsClient *llm.Client, codeModelsClient *llm.Client, jiraClient *atlassian.Client, nvdClient *nvd.Client, sfClient *salesforce.Client, chorusClient *chorus.Client, datadogClients *datadog.MultiClient, awsClient *aws.Client, azureClient *azure.Client, databricksClient *databricks.Client, clickhouseClient *clickhouse.Client, freshworksClient *freshworks.Client, googleClient *google.Client, document360Client *document360.Client, dashboardRegistry *dashboards.Registry, workflowRegistry *workflows.Registry, pp PromptProvider, agentID, appURL string, sessions *SessionStore, maxToolRounds int, userContextStore *UserContextStore, usage UsageRecorder) *Router {
	// Channel-context cache reuses the thread session window so that an
	// active in-thread conversation does not re-fetch Slack history on
	// every turn. Falls back to the package default when sessions is nil.
	cacheTTL := defaultContextCacheTTL
	if sessions != nil {
		if t := sessions.TTL(); t > 0 {
			cacheTTL = t
		}
	}
	return &Router{
		slackClient:       slackClient,
		ghClient:          ghClient,
		modelsClient:      modelsClient,
		codeModelsClient:  codeModelsClient,
		jiraClient:        jiraClient,
		nvdClient:         nvdClient,
		sfClient:          sfClient,
		chorusClient:      chorusClient,
		datadogClients:    datadogClients,
		awsClient:         awsClient,
		azureClient:       azureClient,
		databricksClient:  databricksClient,
		clickhouseClient:  clickhouseClient,
		freshworksClient:  freshworksClient,
		document360Client: document360Client,
		googleClient:      googleClient,
		dashboards:        dashboardRegistry,
		workflows:         workflowRegistry,
		contextProvider:   NewContextProvider(slackClient, cacheTTL),
		prompts:           pp,
		agentID:           agentID,
		appURL:            appURL,
		sessions:          sessions,
		maxToolRounds:     maxToolRounds,
		userContextStore:  userContextStore,
		billing:           usage,
	}
}

// SetMCP makes the tools of registered MCP connectors available to this
// agent's tool loops.
func (r *Router) SetMCP(reg *mcp.Registry) { r.mcp = reg }

// SetPerf records the timing of this agent's turns and tool calls.
func (r *Router) SetPerf(p PerfRecorder) { r.perf = p }

// SetCatalog gives the tool loops semantic search over workflows and
// dashboards.
func (r *Router) SetCatalog(c *catalog.Index) { r.catalog = c }

// SetJournal records this agent's Slack turns while they run, so a turn cut
// short by a restart is answered afterwards instead of leaving the thread on
// "Processing request" forever.
func (r *Router) SetJournal(j *journal.Journal) { r.inflight = j }

// ResumeTurn re-runs an interrupted Slack turn. The journal calls it after a
// restart; the reply lands in the same thread the person is already watching.
func (r *Router) ResumeTurn(channelID, threadTS, userID, text string) {
	log.Printf("[agent=%s user=%s channel=%s thread=%s] resuming interrupted turn", r.agentID, userID, channelID, threadTS)
	r.HandleThreadReply(channelID, threadTS, userID, text)
}

// ToolDefinitions returns the tool schema this agent's tool loop offers with
// the clients configured right now, for the integrations catalogue.
func (r *Router) ToolDefinitions() []llm.Tool {
	return r.newGeneralHandler("", nil).buildTools()
}

// ContextProvider exposes the channel-history cache so callers (e.g.
// main) can attach a background GC sweeper.
func (r *Router) ContextProvider() *ContextProvider { return r.contextProvider }

func (r *Router) Handle(channelID, userID, text, responseURL string) {
	text = strings.TrimSpace(text)
	if text == "" {
		log.Printf("[user=%s channel=%s] empty command received", userID, channelID)
		r.replyError(responseURL, "Please provide a command. Example: `/ovad please debug the latest message in this channel`")
		return
	}

	log.Printf("[agent=%s user=%s channel=%s] received command: %s", r.agentID, userID, channelID, text)

	auditMsg := fmt.Sprintf(":mag: <@%s> requested in <#%s> (agent: %s):\n> %s", userID, channelID, r.agentID, text)
	auditTS, err := r.slackClient.PostMessage(channelID, auditMsg)
	if err != nil {
		log.Printf("[agent=%s user=%s channel=%s] failed to post audit message: %v", r.agentID, userID, channelID, err)
	}

	_ = slack.RespondToURL(responseURL, fmt.Sprintf("Processing request: _%s_", text), true)

	// Register a thread session so follow-up replies are auto-handled.
	// Acquire the processing lock so that any thread reply arriving while
	// the initial request is still running is silently ignored (prevents
	// casual chatter from triggering a second concurrent response).
	var sess *ThreadSession
	if auditTS != "" && r.sessions != nil {
		if sess = r.sessions.Open(channelID, auditTS, userID, r.agentID, r); sess != nil {
			sess.TryStartProcessing()
			defer sess.DoneProcessing()
		}
	}

	userContext := r.resolveUserContext(userID)

	if isIntroIntent(strings.ToLower(text)) {
		log.Printf("[user=%s channel=%s] routed to: intro", userID, channelID)
		// Intro replies go to the channel (not a thread) so the whole team can see them.
		_, _ = r.slackClient.PostMessage(channelID, r.prompts.MustGet("intro"))
		return
	}
	ctx, inflight := r.beginTurn(context.Background(), channelID, auditTS, userID, text)
	defer inflight.Done()
	r.dispatch(ctx, channelID, userID, text, responseURL, auditTS, userContext, sess)

	// Post a session footer so the user knows they can reply in the thread.
	if auditTS != "" && r.sessions != nil {
		if !r.threadAnchorExists(channelID, auditTS) {
			r.sessions.Close(channelID, auditTS, "anchor message deleted")
			return
		}
		ttlMinutes := int(math.Round(r.sessions.TTL().Minutes()))
		footer := fmt.Sprintf("_:thread: Thread session active — reply here for %d min without a /command._", ttlMinutes)
		_ = r.slackClient.PostThreadReply(channelID, auditTS, footer)
	}
}

func isIntroIntent(text string) bool {
	// Exact-match keywords — the entire message must be exactly this.
	exactKeywords := []string{"help", "hi", "hello"}
	trimmed := strings.TrimSpace(text)
	for _, kw := range exactKeywords {
		if trimmed == kw {
			return true
		}
	}
	// Substring-match keywords — safe because they are multi-word and specific.
	substringKeywords := []string{"introduce yourself", "who are you", "what are you", "what can you do", "what do you do"}
	for _, kw := range substringKeywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// slackMessageLinkRe matches Slack message permalinks in already-lowercased
// text. Reading a linked thread needs fetch_thread_context, which only the
// general handler's tool loop can run.
var slackMessageLinkRe = regexp.MustCompile(`https://[^/\s]+\.slack\.com/archives/[a-z0-9]+/p\d{16}`)

func isDebugIntent(text string) bool {
	// If the user requests an action (rerun, modify, create PR, etc.), route to
	// the general handler which has the full tool loop — the debug handler is
	// analysis-only and cannot execute actions.
	if requiresAction(text) {
		return false
	}
	if slackMessageLinkRe.MatchString(text) {
		return false
	}
	// A GitHub Actions workflow run URL is an implicit debug request.
	if len(github.ExtractWorkflowRunURLs(text)) > 0 {
		return true
	}
	debugKeywords := []string{"debug", "analyze", "investigate", "diagnose", "what happened", "explain the error", "look at the latest"}
	for _, kw := range debugKeywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// requiresAction returns true when the user's message asks for a concrete action
// that needs tool access (rerun workflows, modify files, create PRs, etc.).
func requiresAction(text string) bool {
	actionKeywords := []string{
		"rerun", "re-run", "re run", "retry", "restart",
		"create pr", "create a pr", "open pr", "open a pr",
		"modify", "change", "update", "edit", "add", "remove",
		"create ticket", "create a ticket", "create issue", "create a issue",
		"create jira", "create a jira",
	}
	for _, kw := range actionKeywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func (r *Router) newDebugHandler(userContext string) *DebugHandler {
	return &DebugHandler{
		slackClient:      r.slackClient,
		ghClient:         r.ghClient,
		modelsClient:     r.modelsClient,
		contextProvider:  r.contextProvider,
		prompts:          r.prompts,
		userContext:      userContext,
		agentID:          r.agentID,
		userContextStore: r.userContextStore,
	}
}

func (r *Router) newGeneralHandler(userContext string, session *ThreadSession) *GeneralHandler {
	return &GeneralHandler{
		slackClient:       r.slackClient,
		ghClient:          r.ghClient,
		modelsClient:      r.modelsClient,
		codeModelsClient:  r.codeModelsClient,
		jiraClient:        r.jiraClient,
		nvdClient:         r.nvdClient,
		sfClient:          r.sfClient,
		chorusClient:      r.chorusClient,
		datadogClients:    r.datadogClients,
		awsClient:         r.awsClient,
		azureClient:       r.azureClient,
		databricksClient:  r.databricksClient,
		clickhouseClient:  r.clickhouseClient,
		freshworksClient:  r.freshworksClient,
		document360Client: r.document360Client,
		googleClient:      r.googleClient,
		dashboards:        r.dashboards,
		workflows:         r.workflows,
		mcp:               r.mcp,
		contextProvider:   r.contextProvider,
		catalog:           r.catalog,
		prompts:           r.prompts,
		agentID:           r.agentID,
		appURL:            r.appURL,
		maxToolRounds:     r.maxToolRounds,
		userContext:       userContext,
		session:           session,
		userContextStore:  r.userContextStore,
		billing:           r.billing,
		billingSource:     billing.SourceSlack,
		perf:              r.perf,
	}
}

// threadAnchorExists reports whether the message a session thread hangs off is
// still in the channel. A failed check counts as present so a transient Slack
// error never silences a live thread.
func (r *Router) threadAnchorExists(channelID, ts string) bool {
	exists, err := r.slackClient.MessageExists(channelID, ts)
	return err != nil || exists
}

func (r *Router) replyError(responseURL, msg string) {
	if err := slack.RespondToURL(responseURL, msg, true); err != nil {
		log.Printf("failed to send error to user: %v", err)
	}
}

// resolveUserContext fetches Slack profile metadata for the requesting user.
// Returns a human-readable summary that gets injected into the system prompt.
// On failure it returns a minimal fallback so the pipeline never blocks.
func (r *Router) resolveUserContext(userID string) string {
	user, err := r.slackClient.GetUserInfo(userID)
	if err != nil {
		log.Printf("[user=%s] failed to resolve user context: %v", userID, err)
		return fmt.Sprintf("Slack User ID: %s (profile lookup failed)", userID)
	}
	parts := []string{
		fmt.Sprintf("Slack User ID: %s", user.ID),
		fmt.Sprintf("Real Name: %s", user.RealName),
	}
	if user.Profile.DisplayName != "" {
		parts = append(parts, fmt.Sprintf("Display Name: %s", user.Profile.DisplayName))
	}
	if user.Profile.Email != "" {
		parts = append(parts, fmt.Sprintf("Email: %s", user.Profile.Email))
	}
	if user.Profile.Title != "" {
		parts = append(parts, fmt.Sprintf("Title: %s", user.Profile.Title))
	}
	return strings.Join(parts, "\n  ")
}

// HandleThreadReply processes a user message posted in an active session thread.
// It routes through the same command logic as a slash command, replying in-thread.
func (r *Router) HandleThreadReply(channelID, threadTS, userID, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	log.Printf("[agent=%s user=%s channel=%s thread=%s] thread follow-up: %s",
		r.agentID, userID, channelID, threadTS, text)

	userContext := r.resolveUserContext(userID)

	// Look up the session so we can pass it to the handler — this lets the
	// handler reuse branches/PRs created in earlier messages of this thread.
	var sess *ThreadSession
	if r.sessions != nil {
		if !r.threadAnchorExists(channelID, threadTS) {
			r.sessions.Close(channelID, threadTS, "anchor message deleted")
			return
		}
		sess = r.sessions.Lookup(channelID, threadTS)
	}

	ctx, inflight := r.beginTurn(context.Background(), channelID, threadTS, userID, text)
	defer inflight.Done()
	r.dispatch(ctx, channelID, userID, text, "", threadTS, userContext, sess)
}

// SlackJournalKind is the journal kind interrupted Slack turns are recorded
// under.
const SlackJournalKind = "slack-turn"

// beginTurn records the turn in the journal. The message text travels with the
// entry because it is the only thing a resume needs that Slack will not hand
// back: the thread it belongs to is addressed by channel and timestamp.
func (r *Router) beginTurn(ctx context.Context, channelID, threadTS, userID, text string) (context.Context, *journal.Handle) {
	if r.inflight == nil || threadTS == "" {
		return ctx, nil
	}
	return r.inflight.Begin(ctx, SlackJournalKind, channelID+"/"+threadTS, userID+" "+text)
}

// dispatch runs the request through the debug or general handler.
func (r *Router) dispatch(ctx context.Context, channelID, userID, text, responseURL, threadTS, userContext string, sess *ThreadSession) {
	if isDebugIntent(strings.ToLower(text)) {
		log.Printf("[user=%s channel=%s thread=%s] routed to: debug", userID, channelID, threadTS)
		r.newDebugHandler(userContext).Execute(ctx, channelID, userID, text, responseURL, threadTS)
		return
	}
	log.Printf("[user=%s channel=%s thread=%s] routed to: general handler", userID, channelID, threadTS)
	r.newGeneralHandler(userContext, sess).Execute(ctx, channelID, userID, text, responseURL, threadTS)
}

// RunWorkflow runs a workflow's prompt through this agent's headless LLM
// tool-loop. It is called by the workflow scheduler on every tick. There is
// no Slack audit thread, response URL, or channel context — so the prompt
// must be fully self-contained and use tools (post_slack_message, Jira,
// GitHub, etc.) to produce any user-visible side effects.
//
// Returns the final assistant message content or the first tool-loop error.
func (r *Router) RunWorkflow(ctx context.Context, userID, workflowID, workflowName, model, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("workflow prompt is empty")
	}
	// Deliberately do NOT embed the creator's Slack ID in this string. It is
	// injected into the system prompt's "REQUESTING USER IDENTITY" block
	// ({{USER_CONTEXT}}). A scheduled tick has no requesting user, and any Slack
	// ID placed here gets latched onto by the model as a fallback @-mention /
	// assignee / originator whenever it cannot confidently resolve a person from
	// the work item — which surfaced as the workflow creator being rendered as
	// the ticket originator in auto-fix summaries. The creator ID is still passed
	// to ExecuteHeadless below (for operator logging only), never into the model.
	userContext := fmt.Sprintf("Scheduled workflow run (agent=%s). There is NO interactive or requesting user — this tick was triggered by a cron schedule, not a person. "+
		"The words \"me\", \"my\", \"I\", and \"mine\" have no referent here; do NOT attribute anything to a current or session user. "+
		"Every person named in output (ticket originators, assignees, reviewers, Slack @-mentions) MUST be resolved from the specific work item's own data (e.g. Jira changelog, reporter, comments) — NEVER from the run, session, or workflow-creator identity. "+
		"If a person cannot be resolved from the work item, omit them rather than substituting any ambient identity. "+
		"Execute the instruction autonomously using tools.", r.agentID)
	h := r.newGeneralHandler(userContext, nil)
	h.headless = true
	h.billingSource = billing.SourceWorkflow
	h.billingWorkflowID = workflowID
	h.billingWorkflowName = workflowName
	h.modelOverride = model
	return h.ExecuteHeadless(ctx, userID, prompt)
}

// RunDashboardPrompt renders a prompt-driven dashboard: it runs a fully
// substituted prompt through this agent's headless LLM tool-loop and returns
// the model's final Markdown report. It mirrors RunWorkflow (no Slack thread,
// no requesting user identity injected into the model) but is billed under the
// dashboard source and does not post to Slack — the dashboards registry stores
// the returned Markdown on the instance and the HTML view renders it.
func (r *Router) RunDashboardPrompt(ctx context.Context, userID, dashboardID, dashboardName, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("dashboard prompt is empty")
	}
	// Same identity guard as RunWorkflow: a dashboard render has no requesting
	// user, so no Slack ID is placed in the model-visible user context.
	userContext := fmt.Sprintf("Dashboard render (agent=%s). There is NO interactive or requesting user — this was triggered to build a read-only summary dashboard, not by a person speaking. "+
		"The words \"me\", \"my\", \"I\", and \"mine\" have no referent here; do NOT attribute anything to a current or session user. "+
		"Resolve every person named in output from the underlying work item's own data, never from the run or requesting identity. "+
		"Produce the requested report and RETURN it as your final message — do NOT call post_slack_message or any mutating tool.", r.agentID)
	h := r.newGeneralHandler(userContext, nil)
	h.headless = true
	h.billingSource = billing.SourceDashboard
	h.billingWorkflowID = dashboardID
	h.billingWorkflowName = dashboardName
	return h.ExecuteHeadless(ctx, userID, prompt)
}

// chatFallbackUser is the usage-reporting bucket for a web chat turn whose
// sender has no resolvable Slack account, so those turns stay attributable as
// a group rather than being dropped from per-user reporting entirely.
const chatFallbackUser = "ui-chat"

// RunChat runs an interactive UI chat turn through this agent's LLM tool-loop,
// giving the centralized web chat the same tool access (GitHub, Jira, Datadog,
// Databricks, …) as a Slack command. history is the prior transcript (oldest
// first) and userMessage is the new turn. userEmail is the OAuth-proxy-verified
// sender (or "" when no proxy is in front); it lets a created Jira ticket record
// Reporter = the requesting human. userSlackID is that same person's Slack
// member ID when the caller could resolve it, which attributes the turn to them
// in usage reporting and on anything the turn opens (PR body, Jira reporter)
// exactly as a Slack command would; it falls back to the chatFallbackUser
// bucket when the sender has no resolvable Slack account. There is no Slack
// channel or thread, so Slack-only tools are suppressed (headless) and the
// final assistant text is returned to the caller (the chat registry) to persist
// and display.
//
// Returns the reply text or the first tool-loop error.
func (r *Router) RunChat(ctx context.Context, userEmail, userSlackID string, history []llm.ChatMessage, userMessage string, tracker *progress.Tracker) (string, error) {
	if strings.TrimSpace(userMessage) == "" {
		return "", fmt.Errorf("chat message is empty")
	}
	userContext := fmt.Sprintf("Interactive chat with the %s agent through the web UI (no Slack thread). Answer directly and use tools to fetch real data before responding. The only reader is the requesting user themselves, so never write their user ID or a <@...> mention into a reply: here it is inert text, not a ping.", r.agentID)
	h := r.newGeneralHandler(userContext, nil)
	h.headless = true
	h.requesterEmail = userEmail
	h.billingSource = billing.SourceChat
	attrUser := strings.TrimSpace(userSlackID)
	// Only a real member ID is trusted as an identity: anything else would put
	// an unverified name into usage reporting and into whatever the turn opens.
	if !slackUserIDRe.MatchString(attrUser) {
		attrUser = chatFallbackUser
	}
	return h.ExecuteChat(ctx, attrUser, history, userMessage, tracker)
}
