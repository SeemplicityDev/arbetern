package commands

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/aws"
	"github.com/justmike1/arbetern/chorus"
	"github.com/justmike1/arbetern/dashboards"
	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/llm"
	"github.com/justmike1/arbetern/nvd"
	"github.com/justmike1/arbetern/salesforce"
	"github.com/justmike1/arbetern/slack"
	"github.com/justmike1/arbetern/workflows"
)

type Router struct {
	slackClient      SlackClient
	ghClient         *github.Client
	modelsClient     *llm.Client
	codeModelsClient *llm.Client
	jiraClient       *atlassian.Client
	nvdClient        *nvd.Client
	sfClient         *salesforce.Client
	chorusClient     *chorus.Client
	datadogClients   *datadog.MultiClient
	awsClient        *aws.Client
	dashboards       *dashboards.Registry
	workflows        *workflows.Registry
	contextProvider  *ContextProvider
	memory           *ConversationMemory
	prompts          PromptProvider
	agentID          string
	appURL           string
	sessions         *SessionStore
	maxToolRounds    int
	userContextStore *UserContextStore
}

func NewRouter(slackClient SlackClient, ghClient *github.Client, modelsClient *llm.Client, codeModelsClient *llm.Client, jiraClient *atlassian.Client, nvdClient *nvd.Client, sfClient *salesforce.Client, chorusClient *chorus.Client, datadogClients *datadog.MultiClient, awsClient *aws.Client, dashboardRegistry *dashboards.Registry, workflowRegistry *workflows.Registry, pp PromptProvider, agentID, appURL string, sessions *SessionStore, maxToolRounds int, userContextStore *UserContextStore) *Router {
	return &Router{
		slackClient:      slackClient,
		ghClient:         ghClient,
		modelsClient:     modelsClient,
		codeModelsClient: codeModelsClient,
		jiraClient:       jiraClient,
		nvdClient:        nvdClient,
		sfClient:         sfClient,
		chorusClient:     chorusClient,
		datadogClients:   datadogClients,
		awsClient:        awsClient,
		dashboards:       dashboardRegistry,
		workflows:        workflowRegistry,
		contextProvider:  NewContextProvider(slackClient),
		memory:           NewConversationMemory(),
		prompts:          pp,
		agentID:          agentID,
		appURL:           appURL,
		sessions:         sessions,
		maxToolRounds:    maxToolRounds,
		userContextStore: userContextStore,
	}
}

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
	if auditTS != "" && r.sessions != nil {
		r.sessions.Open(channelID, auditTS, userID, r.agentID, r)
		if sess := r.sessions.Lookup(channelID, auditTS); sess != nil {
			sess.TryStartProcessing()
			defer sess.DoneProcessing()
		}
	}

	r.memory.AddUserMessage(channelID, userID, text)

	userContext := r.resolveUserContext(userID)

	// Look up the session so the initial handler can persist branches/PRs
	// for follow-up thread messages.
	var sess *ThreadSession
	if auditTS != "" && r.sessions != nil {
		sess = r.sessions.Lookup(channelID, auditTS)
	}

	// First-class account dashboard subcommand: `dashboard <account> [--refresh]`.
	// Skips the LLM, runs a deterministic pipeline, and posts a Block Kit reply.
	// Gated on having the registry + Salesforce configured — resolving the
	// account by name requires Salesforce.
	if account, refresh, ok := parseDashboardSubcommand(text); ok && r.dashboards != nil && r.sfClient != nil && r.sfClient.Ready() {
		log.Printf("[agent=%s user=%s channel=%s] routed to: account dashboard", r.agentID, userID, channelID)
		r.handleAccountDashboard(channelID, userID, responseURL, auditTS, account, refresh)
		return
	}

	lower := strings.ToLower(text)

	switch {
	case isIntroIntent(lower):
		log.Printf("[user=%s channel=%s] routed to: intro", userID, channelID)
		// Intro replies go to the channel (not a thread) so the whole team can see them.
		_, _ = r.slackClient.PostMessage(channelID, r.prompts.MustGet("intro"))
		return

	case isDebugIntent(lower):
		log.Printf("[user=%s channel=%s] routed to: debug", userID, channelID)
		r.newDebugHandler(userContext).Execute(channelID, userID, text, responseURL, auditTS)

	default:
		log.Printf("[user=%s channel=%s] routed to: general handler", userID, channelID)
		r.newGeneralHandler(userContext, sess).Execute(channelID, userID, text, responseURL, auditTS)
	}

	// Post a session footer so the user knows they can reply in the thread.
	if auditTS != "" && r.sessions != nil {
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

func isDebugIntent(text string) bool {
	// If the user requests an action (rerun, modify, create PR, etc.), route to
	// the general handler which has the full tool loop — the debug handler is
	// analysis-only and cannot execute actions.
	if requiresAction(text) {
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
		memory:           r.memory,
		prompts:          r.prompts,
		userContext:      userContext,
		agentID:          r.agentID,
		userContextStore: r.userContextStore,
	}
}

func (r *Router) newGeneralHandler(userContext string, session *ThreadSession) *GeneralHandler {
	return &GeneralHandler{
		slackClient:      r.slackClient,
		ghClient:         r.ghClient,
		modelsClient:     r.modelsClient,
		codeModelsClient: r.codeModelsClient,
		jiraClient:       r.jiraClient,
		nvdClient:        r.nvdClient,
		sfClient:         r.sfClient,
		chorusClient:     r.chorusClient,
		datadogClients:   r.datadogClients,
		awsClient:        r.awsClient,
		dashboards:       r.dashboards,
		workflows:        r.workflows,
		contextProvider:  r.contextProvider,
		memory:           r.memory,
		prompts:          r.prompts,
		agentID:          r.agentID,
		appURL:           r.appURL,
		maxToolRounds:    r.maxToolRounds,
		userContext:      userContext,
		session:          session,
		userContextStore: r.userContextStore,
	}
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

	r.memory.AddUserMessage(channelID, userID, text)

	userContext := r.resolveUserContext(userID)

	// Look up the session so we can pass it to the handler — this lets the
	// handler reuse branches/PRs created in earlier messages of this thread.
	var sess *ThreadSession
	if r.sessions != nil {
		sess = r.sessions.Lookup(channelID, threadTS)
	}

	lower := strings.ToLower(text)

	switch {
	case isDebugIntent(lower):
		log.Printf("[user=%s channel=%s thread=%s] thread routed to: debug", userID, channelID, threadTS)
		r.newDebugHandler(userContext).Execute(channelID, userID, text, "", threadTS)

	default:
		log.Printf("[user=%s channel=%s thread=%s] thread routed to: general handler", userID, channelID, threadTS)
		r.newGeneralHandler(userContext, sess).Execute(channelID, userID, text, "", threadTS)
	}
}

// RunWorkflow runs a workflow's prompt through this agent's headless LLM
// tool-loop. It is called by the workflow scheduler on every tick. There is
// no Slack audit thread, response URL, or channel context — so the prompt
// must be fully self-contained and use tools (post_slack_message, Jira,
// GitHub, etc.) to produce any user-visible side effects.
//
// Returns the final assistant message content or the first tool-loop error.
func (r *Router) RunWorkflow(ctx context.Context, userID, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("workflow prompt is empty")
	}
	userContext := fmt.Sprintf("Scheduled workflow run (agent=%s, creator=%s). No interactive user is present — execute the instruction autonomously using tools.", r.agentID, userID)
	h := r.newGeneralHandler(userContext, nil)
	h.headless = true
	return h.ExecuteHeadless(ctx, userID, prompt)
}
