package commands

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/aws"
	"github.com/justmike1/arbetern/azure"
	"github.com/justmike1/arbetern/billing"
	"github.com/justmike1/arbetern/chorus"
	"github.com/justmike1/arbetern/clickhouse"
	"github.com/justmike1/arbetern/config"
	"github.com/justmike1/arbetern/dashboards"
	"github.com/justmike1/arbetern/databricks"
	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/freshworks"
	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/llm"
	"github.com/justmike1/arbetern/nvd"
	"github.com/justmike1/arbetern/salesforce"
	"github.com/justmike1/arbetern/workflows"
)

// knownExtensions is the set of all known programming-language file extensions
// sourced from github-linguist/linguist. Built once at init time.
var knownExtensions map[string]bool

// maxFileContentLen is the character limit for file content returned by the
// get_file_content tool. Files longer than this are truncated with a warning.
const maxFileContentLen = 32000

// extensionRe matches tokens that look like file extensions (e.g. ".yaml", ".go")
// or filenames with extensions (e.g. "main.go", "deploy.yaml") in user text.
var extensionRe = regexp.MustCompile(`(?i)\.[a-z0-9_][a-z0-9_.]*`)

func init() {
	knownExtensions = make(map[string]bool, 1500)
	var exts []string
	if err := json.Unmarshal([]byte(config.ExtensionsRaw), &exts); err != nil {
		// Empty or missing file — extension detection disabled.
		return
	}
	for _, ext := range exts {
		knownExtensions[strings.ToLower(ext)] = true
	}
}

type GeneralHandler struct {
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
	azureClient      *azure.Client
	databricksClient *databricks.Client
	clickhouseClient *clickhouse.Client
	freshworksClient *freshworks.Client
	dashboards       *dashboards.Registry
	workflows        *workflows.Registry
	contextProvider  *ContextProvider
	memory           *ConversationMemory
	prompts          PromptProvider
	agentID          string
	appURL           string
	maxToolRounds    int
	userContext      string
	currentChannelID string
	currentAuditTS   string
	branchMgr        *BranchManager
	session          *ThreadSession
	userContextStore *UserContextStore
	// billing records the token cost of each turn for the Usage & Billing
	// tab. Optional — nil disables tracking. billingSource and the workflow
	// fields tag the recorded event with its entry path.
	billing             UsageRecorder
	billingSource       string
	billingWorkflowID   string
	billingWorkflowName string
	// modelOverride, when set, replaces the default CODE_MODEL for a headless
	// workflow tick with a specific backend deployment (workflow.Model).
	modelOverride string
	// aggregateCache holds the structured results of datadog_logs_aggregate
	// calls made during this run, keyed by the short ID returned to the LLM
	// (agg_1, agg_2, …). It lets upload_aggregate_csv assemble a CSV file
	// server-side so the model never has to re-emit the full dataset inline
	// (which it does unreliably for large tables). Per-run state: a fresh
	// handler is created for each command/tick by newGeneralHandler.
	aggregateCache map[string]cachedAggregate
	// postedSlackSigs deduplicates post_slack_message calls within a single
	// headless run, keyed by a (channel, thread_ts, text) signature → the ts
	// of the message that was actually posted. A scheduled tick's LLM loop
	// occasionally calls post_slack_message twice for the same report, which
	// produces duplicate top-level messages in the channel. The guard performs
	// the real post once and returns the original ts for any exact repeat, so
	// the model still has a valid thread parent for its threaded reply. Per-run
	// state: a fresh handler is created for each tick by newGeneralHandler.
	postedSlackSigs map[string]string
	// headless is true when the handler is executing a scheduled workflow
	// tick rather than a Slack-driven command. Affects tool gating (e.g.
	// reply_in_thread is suppressed) and suppresses audit messaging.
	headless bool
	// requesterEmail is the OAuth-proxy-verified email of the web-chat user,
	// set only for interactive chat turns (which are also headless). Empty for
	// Slack commands and scheduled workflows. Used to attribute a created Jira
	// ticket's Reporter to the requester when there is no Slack identity.
	requesterEmail string
}

// cachedAggregate is one datadog_logs_aggregate result retained for the
// duration of a run so upload_aggregate_csv can render it to CSV without the
// model re-emitting the data.
type cachedAggregate struct {
	query   string
	results []datadog.PercentileResult
}

// groupBySpec accepts the datadog_logs_aggregate "group_by" argument as either
// a single facet string, a comma-separated string, or a JSON array of facets.
// Multiple facets produce a composite breakdown (one row per facet-value
// combination). Empty input leaves the slice nil, so the datadog layer groups
// nothing (a single overall bucket).
type groupBySpec []string

func (g *groupBySpec) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []string
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		*g = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*g = groupBySpec{s}
	return nil
}

// resolveUploadChannel picks the Slack channel a file upload should be shared
// to. It prefers an explicit channel_id from the tool args, then the channel
// the command is running in, then the handler's current channel. In a headless
// workflow tick all of those are empty unless the model passes channel_id, so
// the caller must treat an empty result as a precondition error.
func (h *GeneralHandler) resolveUploadChannel(channelID, argChannelID string) string {
	if c := strings.TrimSpace(argChannelID); c != "" {
		return c
	}
	if c := strings.TrimSpace(channelID); c != "" {
		return c
	}
	return strings.TrimSpace(h.currentChannelID)
}

// resolveUploadThread picks the thread timestamp a file upload should be posted
// under. It prefers an explicit thread_ts from the tool args, then the current
// audit thread (Slack-driven commands). Empty means top-level channel post.
func (h *GeneralHandler) resolveUploadThread(argThreadTS string) string {
	if t := strings.TrimSpace(argThreadTS); t != "" {
		return t
	}
	return strings.TrimSpace(h.currentAuditTS)
}

// cacheAggregate stores a structured aggregate result under a fresh per-run ID
// (agg_1, agg_2, …) and returns that ID.
func (h *GeneralHandler) cacheAggregate(query string, results []datadog.PercentileResult) string {
	if h.aggregateCache == nil {
		h.aggregateCache = make(map[string]cachedAggregate)
	}
	id := fmt.Sprintf("agg_%d", len(h.aggregateCache)+1)
	h.aggregateCache[id] = cachedAggregate{query: query, results: results}
	return id
}

// buildAggregateCSV assembles a single CSV from the named cached aggregate
// results. Columns: site,group_by,key,measure,count,p50,p95,p99 — one row per
// (site, group key) across every requested aggregate. Returns an error string
// (empty on success) and the CSV content.
func (h *GeneralHandler) buildAggregateCSV(ids []string) (string, string) {
	if len(ids) == 0 {
		return "Error: no aggregate_ids provided.", ""
	}
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.Write([]string{"site", "group_by", "key", "measure", "count", "p50", "p95", "p99"})
	rows := 0
	for _, id := range ids {
		id = strings.TrimSpace(id)
		cached, ok := h.aggregateCache[id]
		if !ok {
			return fmt.Sprintf("Error: aggregate_id %q not found. Valid ids are the ones returned by datadog_logs_aggregate calls in this run.", id), ""
		}
		for _, res := range cached.results {
			for _, r := range res.Rows {
				_ = w.Write([]string{
					res.Site,
					res.GroupBy,
					r.Key,
					res.Measure,
					strconv.FormatFloat(r.Count, 'f', 0, 64),
					strconv.FormatFloat(r.P50, 'f', 3, 64),
					strconv.FormatFloat(r.P95, 'f', 3, 64),
					strconv.FormatFloat(r.P99, 'f', 3, 64),
				})
				rows++
			}
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Sprintf("Error building CSV: %v", err), ""
	}
	if rows == 0 {
		return "Error: the named aggregate result(s) contained no rows.", ""
	}
	return "", sb.String()
}

// recordUsage tags the cumulative token usage of a finished turn with the
// handler's agent and entry-path metadata and hands it to the billing store.
// No-op when billing is disabled or no tokens were consumed. comp carries the
// Headroom compression savings accumulated across the turn's LLM rounds.
func (h *GeneralHandler) recordUsage(model, userID string, u llm.Usage, comp llm.CompressionStats) {
	if h.billing == nil || u.TotalTokens == 0 {
		return
	}
	src := h.billingSource
	if src == "" {
		src = billing.SourceSlack
	}
	// Attribute interactive turns (Slack / chat) to the human who prompted
	// them; scheduled workflow ticks are attributed via the workflow name, not
	// the creator, so the user ID is omitted for them.
	attrUser := ""
	if src == billing.SourceSlack || src == billing.SourceChat {
		attrUser = userID
	}
	h.billing.Record(billing.Event{
		Agent:                  h.agentID,
		Source:                 src,
		UserID:                 attrUser,
		WorkflowID:             h.billingWorkflowID,
		WorkflowName:           h.billingWorkflowName,
		Model:                  model,
		PromptTokens:           u.PromptTokens,
		CachedPromptTokens:     u.CachedPromptTokens,
		CompletionTokens:       u.CompletionTokens,
		TotalTokens:            u.TotalTokens,
		CompressionInputTokens: comp.TokensBefore,
		CompressionSavedTokens: comp.TokensSaved,
	})
}

func (h *GeneralHandler) Execute(channelID, userID, text, responseURL, auditTS string) {
	ctx := context.Background()
	h.currentChannelID = channelID
	h.currentAuditTS = auditTS
	h.aggregateCache = nil
	h.branchMgr = NewBranchManager(h.ghClient, h.agentID, h.session)

	tools := h.buildTools()

	channelContext := ""
	if cc, err := h.contextProvider.GetChannelContext(channelID); err == nil {
		channelContext = cc
	}

	// Choose the active LLM client: use the code model when the request
	// involves code changes (PRs, file modifications, etc.).
	activeClient := h.modelsClient
	if h.codeModelsClient != nil && isCodeIntent(strings.ToLower(text)) {
		activeClient = h.codeModelsClient
		log.Printf("[user=%s channel=%s] using code model (%s) for code-related request",
			userID, channelID, h.codeModelsClient.Model())
	}

	systemMsg := h.systemPrompt()
	systemMsg = strings.Replace(systemMsg, "{{MODEL}}", activeClient.Model(), 1)
	systemMsg = strings.Replace(systemMsg, "{{USER_ID}}", userID, 1)
	systemMsg = strings.Replace(systemMsg, "{{USER_CONTEXT}}", h.userContext, 1)
	history := h.memory.GetHistory(channelID, userID)
	if history != "" {
		systemMsg += fmt.Sprintf("\n\nPrevious conversation with this user:\n%s", history)
	}
	if persistent := h.readPersistentUserContext(userID); persistent != "" {
		systemMsg += fmt.Sprintf("\n\nRecurring topics this user has asked about previously (may hint at current intent):\n%s", persistent)
	}
	if channelContext != "" && channelContext != "(no recent messages)" {
		systemMsg += fmt.Sprintf("\n\nRecent channel messages for context:\n%s", channelContext)
	}

	// Proactively fetch workflow run logs from GitHub Actions URLs found in the user's message
	// (not channel context — channel context may contain unrelated CI notifications).
	if workflowLogs := h.fetchWorkflowLogs(ctx, text, userID, channelID); workflowLogs != "" {
		systemMsg += fmt.Sprintf("\n\nGitHub Actions workflow run details and logs (auto-fetched from URLs found in your message):\n\n%s", workflowLogs)
	}

	messages := []llm.ChatMessage{
		llm.NewChatMessage("system", systemMsg),
		llm.NewChatMessage("user", text),
	}

	repliedInThread := false
	toolCallsMade := false
	blockedPreActionAcks := 0
	emptyResponseRetries := 0

	// Track cumulative token usage and compression savings across all LLM rounds.
	var totalUsage llm.Usage
	var totalCompression llm.CompressionStats
	defer func() { h.recordUsage(activeClient.Model(), userID, totalUsage, totalCompression) }()

	rounds := h.maxToolRounds
	if rounds <= 0 {
		rounds = 50
	}

	for i := 0; i < rounds; i++ {
		resp, err := activeClient.CompleteWithTools(ctx, messages, tools)
		if err != nil {
			log.Printf("[user=%s channel=%s] LLM completion failed for general query: %v", userID, channelID, err)
			h.replyDefault(channelID, responseURL, auditTS, fmt.Sprintf("Failed to process request: %v", err))
			return
		}

		if resp.Usage != nil {
			totalUsage.PromptTokens += resp.Usage.PromptTokens
			totalUsage.CachedPromptTokens += resp.Usage.CachedPromptTokens
			totalUsage.CompletionTokens += resp.Usage.CompletionTokens
			totalUsage.TotalTokens += resp.Usage.TotalTokens
		}
		if resp.Compression != nil {
			totalCompression.TokensBefore += resp.Compression.TokensBefore
			totalCompression.TokensAfter += resp.Compression.TokensAfter
			totalCompression.TokensSaved += resp.Compression.TokensSaved
		}

		if len(resp.Choices) == 0 {
			log.Printf("[user=%s channel=%s] LLM returned no choices", userID, channelID)
			h.replyDefault(channelID, responseURL, auditTS, "No response from the model.")
			return
		}

		choice := resp.Choices[0]

		if len(choice.Message.ToolCalls) == 0 {
			// Guardrail: prevent placeholder acknowledgements like "I'm checking..."
			// from being posted for code/repo actions before any tool execution.
			if !toolCallsMade && isCodeIntent(strings.ToLower(text)) && isPreActionAck(choice.Message.Content) {
				blockedPreActionAcks++
				log.Printf("[user=%s channel=%s] blocked pre-action ack reply (%d); forcing tool execution retry", userID, channelID, blockedPreActionAcks)
				if blockedPreActionAcks >= 3 {
					h.replyDefault(channelID, responseURL, auditTS, "I couldn't complete this yet because tool execution did not start. Please retry the same request; I'll return only completed results from tool outputs.")
					return
				}
				messages = append(messages,
					llm.NewChatMessage("assistant", choice.Message.Content),
					llm.NewChatMessage("user", "Do not send pre-action acknowledgements. Start tool execution now. Return only completed results from tool outputs (or a concrete tool error after attempted execution)."),
				)
				continue
			}

			// Guardrail: if the model returned an empty response, retry once
			// asking it to produce actual content. This can happen when the
			// Responses API returns no output_text items.
			if strings.TrimSpace(choice.Message.Content) == "" && emptyResponseRetries < 2 {
				emptyResponseRetries++
				log.Printf("[user=%s channel=%s] model returned empty content (retry %d); nudging for a real response", userID, channelID, emptyResponseRetries)
				messages = append(messages,
					llm.NewChatMessage("assistant", ""),
					llm.NewChatMessage("user", "Your previous response was empty. Please provide a complete answer to my original request. Use the available tools if needed."),
				)
				continue
			}

			log.Printf("[user=%s channel=%s] general query completed successfully", userID, channelID)
			h.memory.SetAssistantResponse(channelID, userID, choice.Message.Content)
			h.persistUserContext(userID, text, choice.Message.Content)
			stamp := llm.FormatUsageStamp(&totalUsage, activeClient.Model())
			// If we already replied in a specific thread, don't send a redundant follow-up.
			if repliedInThread {
				log.Printf("[user=%s channel=%s] skipping reply (already replied in thread)", userID, channelID)
				if stamp != "" {
					_ = h.slackClient.PostThreadReply(channelID, auditTS, stamp)
				}
				return
			}
			h.replyDefault(channelID, responseURL, auditTS, choice.Message.Content+stamp)
			return
		}

		messages = append(messages, llm.ChatMessage{
			Role:      "assistant",
			ToolCalls: choice.Message.ToolCalls,
		})

		for _, tc := range choice.Message.ToolCalls {
			log.Printf("[user=%s channel=%s] LLM called tool: %s(%s)", userID, channelID, tc.Function.Name, tc.Function.Arguments)
			toolCallsMade = true
			result := h.executeTool(ctx, channelID, userID, auditTS, tc.Function.Name, tc.Function.Arguments)
			result = stripPreconditionPrefix(result)
			messages = append(messages, llm.NewToolResultMessage(tc.ID, result))
			if tc.Function.Name == "reply_in_thread" && !strings.HasPrefix(result, "Error") {
				repliedInThread = true
			}
			// Dynamically switch to the code model once code-related
			// tools are invoked (covers cases where initial intent detection
			// didn't trigger the code model).
			codeTools := map[string]bool{
				"modify_file": true, "create_file": true, "regex_replace_file": true,
				"get_file_content": true,
				"search_code":      true, "search_code_org": true, "search_files": true,
				"list_directory": true, "get_pull_request": true,
				// Dashboard / workflow composition benefits from the stronger model.
				"create_dashboard": true, "create_workflow": true, "update_workflow": true, "call_workflow": true,
			}
			if codeTools[tc.Function.Name] && h.codeModelsClient != nil && activeClient != h.codeModelsClient {
				activeClient = h.codeModelsClient
				log.Printf("[user=%s channel=%s] switched to code model (%s) after %s call",
					userID, channelID, h.codeModelsClient.Model(), tc.Function.Name)
			}
		}
	}

	log.Printf("[user=%s channel=%s] exceeded max tool rounds", userID, channelID)
	h.replyDefault(channelID, responseURL, auditTS, "The request required too many steps. Please try a simpler query.")
}

// ExecuteHeadless runs the agent's LLM tool-loop against a single prompt,
// with no Slack audit thread, response URL, or channel context. Intended for
// scheduled workflow ticks: the prompt is expected to be fully self-contained
// and drive all side effects through tools (post_slack_message, create_pr,
// Jira, etc.). Returns the final assistant message (or the first tool-loop
// error) — the caller persists this as the run result.
func (h *GeneralHandler) ExecuteHeadless(ctx context.Context, userID, prompt string) (string, error) {
	h.currentChannelID = ""
	h.currentAuditTS = ""
	h.aggregateCache = nil
	h.postedSlackSigs = nil
	h.branchMgr = NewBranchManager(h.ghClient, h.agentID, nil)

	tools := h.buildTools()

	// Scheduled/triggered workflow ticks and call_workflow chains always use
	// the CODE_MODEL when available — the execution needs structured,
	// tool-heavy reasoning (PRs, JSON construction, tool composition) rather
	// than conversational chit-chat, which is the general model's domain.
	// If no dedicated code model is configured the general model is used as
	// a fallback and a warning is logged so operators can notice the drift.
	//
	// A per-workflow Model override (h.modelOverride) wins over CODE_MODEL. The
	// chosen client is captured once as codeClient and reused for the whole
	// tick so the mid-loop code-tool switch can never revert the override.
	codeClient := h.codeModelsClient
	if codeClient == nil {
		codeClient = h.modelsClient
	}
	if h.modelOverride != "" {
		codeClient = codeClient.WithModel(h.modelOverride)
	}
	activeClient := codeClient
	switch {
	case h.modelOverride != "":
		log.Printf("[workflow user=%s agent=%s] using model override (%s) for headless execution",
			userID, h.agentID, activeClient.Model())
	case h.codeModelsClient != nil:
		log.Printf("[workflow user=%s agent=%s] using code model (%s) for headless execution",
			userID, h.agentID, activeClient.Model())
	default:
		log.Printf("[workflow user=%s agent=%s] WARNING: no code model configured, falling back to general model (%s)",
			userID, h.agentID, activeClient.Model())
	}

	systemMsg := h.systemPrompt()
	systemMsg = strings.Replace(systemMsg, "{{MODEL}}", activeClient.Model(), 1)
	// A scheduled tick has no requesting Slack user. Blank out {{USER_ID}} rather
	// than substituting the workflow creator's ID: agent prompts that reference
	// {{USER_ID}} treat it as "the current user", and leaking the creator there
	// makes the model use that ID as a fallback @-mention / assignee. The creator
	// ID is retained in `userID` purely for the operator logs below.
	systemMsg = strings.Replace(systemMsg, "{{USER_ID}}", "", 1)
	systemMsg = strings.Replace(systemMsg, "{{USER_CONTEXT}}", h.userContext, 1)
	systemMsg += "\n\n[SCHEDULED WORKFLOW CONTEXT]\n" +
		"You are executing a scheduled workflow tick. There is NO interactive Slack thread for this run. " +
		"Do not emit placeholder acknowledgements — call tools immediately and return only concrete outcomes. " +
		"To notify a Slack channel, use post_slack_message with an explicit channel_id. " +
		"Do not call create_workflow / update_workflow / delete_workflow from inside a tick."

	messages := []llm.ChatMessage{
		llm.NewChatMessage("system", systemMsg),
		llm.NewChatMessage("user", prompt),
	}

	rounds := h.maxToolRounds
	if rounds <= 0 {
		rounds = 50
	}
	toolCallsMade := 0
	// mutatingTools are the side-effect-producing tools whose failure means
	// this tick did not achieve what the workflow was supposed to do. A
	// failed modify_file (e.g. 404 because the model passed a non-existent
	// branch, or old_content didn't match) means no PR was opened; a failed
	// post_slack_message means no notification went out. We count those
	// specifically so the workflow layer can treat the tick as failed —
	// even when the LLM's final assistant message is a nicely-worded
	// "I tried but…" string — and eventually auto-disable the workflow.
	mutatingTools := map[string]bool{
		"modify_file":        true,
		"create_file":        true,
		"regex_replace_file": true,
		"post_slack_message": true,
		"create_dashboard":   true,
		"create_workflow":    true,
		"update_workflow":    true,
		"delete_workflow":    true,
		"call_workflow":      true,
	}
	var mutatingFailures []string
	var totalUsage llm.Usage
	var totalCompression llm.CompressionStats
	defer func() { h.recordUsage(activeClient.Model(), userID, totalUsage, totalCompression) }()
	for i := 0; i < rounds; i++ {
		resp, err := activeClient.CompleteWithTools(ctx, messages, tools)
		if err != nil {
			log.Printf("[workflow user=%s agent=%s] LLM completion failed after %d rounds / %d tool calls: %v",
				userID, h.agentID, i, toolCallsMade, err)
			return "", fmt.Errorf("LLM completion failed: %w", err)
		}
		if resp.Usage != nil {
			totalUsage.PromptTokens += resp.Usage.PromptTokens
			totalUsage.CachedPromptTokens += resp.Usage.CachedPromptTokens
			totalUsage.CompletionTokens += resp.Usage.CompletionTokens
			totalUsage.TotalTokens += resp.Usage.TotalTokens
		}
		if resp.Compression != nil {
			totalCompression.TokensBefore += resp.Compression.TokensBefore
			totalCompression.TokensAfter += resp.Compression.TokensAfter
			totalCompression.TokensSaved += resp.Compression.TokensSaved
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("LLM returned no choices")
		}
		choice := resp.Choices[0]
		if len(choice.Message.ToolCalls) == 0 {
			final := strings.TrimSpace(choice.Message.Content)
			// Make headless completion visible in the operator log. Without
			// this, a tick that silently ends with "I was unable to…" (or
			// an empty message) is indistinguishable from a tick that
			// actually shipped a PR or Slack message.
			preview := final
			if len(preview) > 200 {
				preview = preview[:200] + "…"
			}
			preview = strings.ReplaceAll(preview, "\n", " ")
			log.Printf("[workflow user=%s agent=%s] completed after %d rounds / %d tool calls; final (%d chars): %q",
				userID, h.agentID, i+1, toolCallsMade, len(final), preview)
			if len(mutatingFailures) > 0 {
				// At least one side-effect tool failed. The tick technically
				// reached a final assistant message, but the intent of the
				// workflow (open a PR, post a Slack message, …) wasn't
				// actually achieved. Surface this as an error so the
				// workflow registry counts the tick as failed and can
				// auto-disable after repeated failures.
				last := mutatingFailures[len(mutatingFailures)-1]
				return final, fmt.Errorf("workflow tick had %d failed mutating tool call(s); last: %s", len(mutatingFailures), last)
			}
			return final, nil
		}
		messages = append(messages, llm.ChatMessage{
			Role:      "assistant",
			ToolCalls: choice.Message.ToolCalls,
		})
		for _, tc := range choice.Message.ToolCalls {
			toolCallsMade++
			log.Printf("[workflow user=%s agent=%s] tool: %s(%s)", userID, h.agentID, tc.Function.Name, tc.Function.Arguments)
			result := h.executeTool(ctx, "", userID, "", tc.Function.Name, tc.Function.Arguments)
			// Precondition errors mean the tool rejected the call BEFORE any
			// side effect was attempted (missing args, content guards, etc.).
			// They are recoverable by the LLM's own retry loop and must not
			// be counted as mutating failures — nothing externally observable
			// happened. We still log them for operator visibility, then strip
			// the sentinel before handing the message back to the LLM.
			precondition := isPreconditionErr(result)
			if precondition {
				result = stripPreconditionPrefix(result)
			}
			// Surface tool-level errors in the operator log. Without this, a
			// silent "Error: old_content not found" (or similar) from a
			// modify_file call is only visible to the LLM and can cause it
			// to ship a weaker fallback (or no-op) PR on its next attempt.
			if strings.HasPrefix(result, "Error") {
				preview := strings.ReplaceAll(result, "\n", " ")
				if len(preview) > 300 {
					preview = preview[:300] + "…"
				}
				if precondition {
					log.Printf("[workflow user=%s agent=%s] tool %s precondition rejected (no side effect attempted): %s", userID, h.agentID, tc.Function.Name, preview)
				} else {
					log.Printf("[workflow user=%s agent=%s] tool %s returned: %s", userID, h.agentID, tc.Function.Name, preview)
					if mutatingTools[tc.Function.Name] {
						mutatingFailures = append(mutatingFailures, fmt.Sprintf("%s: %s", tc.Function.Name, preview))
					}
				}
			}
			messages = append(messages, llm.NewToolResultMessage(tc.ID, result))
			codeTools := map[string]bool{
				"modify_file": true, "create_file": true, "regex_replace_file": true,
				"get_file_content": true,
				"search_code":      true, "search_code_org": true, "search_files": true,
				"list_directory": true, "get_pull_request": true,
				"create_dashboard": true, "create_workflow": true, "update_workflow": true, "call_workflow": true,
			}
			if codeTools[tc.Function.Name] && activeClient != codeClient {
				activeClient = codeClient
			}
		}
	}
	log.Printf("[workflow user=%s agent=%s] exceeded max tool rounds (%d); %d tool calls made",
		userID, h.agentID, rounds, toolCallsMade)
	return "", fmt.Errorf("workflow tick exceeded max tool rounds (%d)", rounds)
}

// ExecuteChat runs the agent's LLM tool-loop for an interactive UI chat turn
// and returns the final assistant message. It seeds the conversation with the
// prior transcript (history, oldest first) followed by the new user message,
// then runs the same tool loop — and therefore the same integrations (GitHub,
// Jira, Datadog, Databricks, …) — that a Slack command gets. Like a headless
// run there is no Slack audit thread, response URL, or channel, so Slack-only
// tools (reply_in_thread, …) are suppressed via h.headless and the reply is
// returned to the caller (the chat registry) to persist and display rather
// than posted to Slack.
//
// It uses the general (conversational) model, upgrading to the code model only
// after a code-related tool is invoked — mirroring the interactive Execute
// path. Returns the reply text or the first tool-loop error.
func (h *GeneralHandler) ExecuteChat(ctx context.Context, userID string, history []llm.ChatMessage, userMessage string) (string, error) {
	h.currentChannelID = ""
	h.currentAuditTS = ""
	h.aggregateCache = nil
	h.postedSlackSigs = nil
	h.branchMgr = NewBranchManager(h.ghClient, h.agentID, nil)

	tools := h.buildTools()

	activeClient := h.modelsClient
	if activeClient == nil {
		return "", fmt.Errorf("LLM client is not configured")
	}

	systemMsg := h.systemPrompt()
	systemMsg = strings.Replace(systemMsg, "{{MODEL}}", activeClient.Model(), 1)
	systemMsg = strings.Replace(systemMsg, "{{USER_ID}}", userID, 1)
	systemMsg = strings.Replace(systemMsg, "{{USER_CONTEXT}}", h.userContext, 1)

	messages := make([]llm.ChatMessage, 0, len(history)+2)
	messages = append(messages, llm.NewChatMessage("system", systemMsg))
	messages = append(messages, history...)
	messages = append(messages, llm.NewChatMessage("user", userMessage))

	rounds := h.maxToolRounds
	if rounds <= 0 {
		rounds = 50
	}
	emptyResponseRetries := 0
	var totalUsage llm.Usage
	var totalCompression llm.CompressionStats
	defer func() { h.recordUsage(activeClient.Model(), userID, totalUsage, totalCompression) }()
	for i := 0; i < rounds; i++ {
		resp, err := activeClient.CompleteWithTools(ctx, messages, tools)
		if err != nil {
			log.Printf("[chat user=%s agent=%s] LLM completion failed after %d rounds: %v", userID, h.agentID, i, err)
			return "", fmt.Errorf("LLM completion failed: %w", err)
		}
		if resp.Usage != nil {
			totalUsage.PromptTokens += resp.Usage.PromptTokens
			totalUsage.CachedPromptTokens += resp.Usage.CachedPromptTokens
			totalUsage.CompletionTokens += resp.Usage.CompletionTokens
			totalUsage.TotalTokens += resp.Usage.TotalTokens
		}
		if resp.Compression != nil {
			totalCompression.TokensBefore += resp.Compression.TokensBefore
			totalCompression.TokensAfter += resp.Compression.TokensAfter
			totalCompression.TokensSaved += resp.Compression.TokensSaved
		}
		if resp.Error != nil {
			return "", fmt.Errorf("%s", resp.Error.Message)
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("LLM returned no choices")
		}
		choice := resp.Choices[0]

		if len(choice.Message.ToolCalls) == 0 {
			final := strings.TrimSpace(choice.Message.Content)
			// Retry once or twice on an empty final response — the Responses
			// API occasionally returns no output_text items.
			if final == "" && emptyResponseRetries < 2 {
				emptyResponseRetries++
				log.Printf("[chat user=%s agent=%s] empty response (retry %d); nudging for content", userID, h.agentID, emptyResponseRetries)
				messages = append(messages,
					llm.NewChatMessage("assistant", ""),
					llm.NewChatMessage("user", "Your previous response was empty. Please provide a complete answer to my request, using the available tools if needed."),
				)
				continue
			}
			log.Printf("[chat user=%s agent=%s] completed after %d rounds", userID, h.agentID, i+1)
			return final, nil
		}

		messages = append(messages, llm.ChatMessage{
			Role:      "assistant",
			ToolCalls: choice.Message.ToolCalls,
		})
		for _, tc := range choice.Message.ToolCalls {
			log.Printf("[chat user=%s agent=%s] tool: %s(%s)", userID, h.agentID, tc.Function.Name, tc.Function.Arguments)
			result := h.executeTool(ctx, "", userID, "", tc.Function.Name, tc.Function.Arguments)
			result = stripPreconditionPrefix(result)
			messages = append(messages, llm.NewToolResultMessage(tc.ID, result))
			codeTools := map[string]bool{
				"modify_file": true, "create_file": true, "regex_replace_file": true,
				"get_file_content": true,
				"search_code":      true, "search_code_org": true, "search_files": true,
				"list_directory": true, "get_pull_request": true,
				"create_dashboard": true, "create_workflow": true, "update_workflow": true, "call_workflow": true,
			}
			if codeTools[tc.Function.Name] && h.codeModelsClient != nil && activeClient != h.codeModelsClient {
				activeClient = h.codeModelsClient
				log.Printf("[chat user=%s agent=%s] switched to code model (%s) after %s call",
					userID, h.agentID, h.codeModelsClient.Model(), tc.Function.Name)
			}
		}
	}
	log.Printf("[chat user=%s agent=%s] exceeded max tool rounds (%d)", userID, h.agentID, rounds)
	return "", fmt.Errorf("chat exceeded max tool rounds (%d)", rounds)
}

func (h *GeneralHandler) systemPrompt() string {
	return h.prompts.SystemPrompt("general")
}

func (h *GeneralHandler) buildTools() []llm.Tool {
	tools := []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolListOrgRepos,
				Description: "List all repositories in the GitHub organization that the bot has access to. Use ONLY when the user explicitly asks to list/discover repos, or when no repository was provided and repo discovery is required.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolListUserRepos,
				Description: "List all repositories accessible by the authenticated GitHub user. Use ONLY when the user explicitly asks to list/discover repos, or when no repository was provided and repo discovery is required.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolListRepoTeams,
				Description: "List the GitHub teams that have direct access to a repository, with each team's permission level (admin, maintain, push, triage, or pull). This mirrors Settings → Collaborators and teams → Direct access in the GitHub UI. Use this to answer 'who can trigger / merge / admin <repo>'. Requires the bot's token to have read:org plus access to the repo; a 403 from GitHub usually means the token lacks read:org or the repo is private to another team. For more than ~3 repos, prefer `list_repo_teams_bulk` instead (parallel fetch).",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"}
					},
					"required":["repo"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolListRepoTeamsBulk,
				Description: "Parallel version of list_repo_teams. Pass an array of repo names (without owner); GitHub is queried with up to 8 concurrent requests and a single combined response is returned. Per-repo errors (404 for missing-admin, etc.) are reported inline alongside the successful entries — they do NOT abort the whole call. Use this whenever you need teams for more than a handful of repos at once; calling list_repo_teams in a loop is strictly slower.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repos":{"type":"array","items":{"type":"string"},"description":"List of repository names (without owner). Duplicates are deduplicated server-side."}
					},
					"required":["repos"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetFilesBulk,
				Description: "Parallel version of get_file_content. Pass an array of {repo, path, branch?} specs; GitHub is queried with up to 8 concurrent requests and a single combined response is returned. Per-file errors are reported inline alongside the successful entries — they do NOT abort the whole call. Use this whenever you need more than a handful of files at once; calling get_file_content in a loop is strictly slower. Each file is independently truncated to 32000 chars.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"files":{
							"type":"array",
							"description":"List of files to fetch.",
							"items":{
								"type":"object",
								"properties":{
									"repo":{"type":"string","description":"Repository name (without owner)"},
									"path":{"type":"string","description":"File path within the repo"},
									"branch":{"type":"string","description":"Optional branch (default: repo default branch)"}
								},
								"required":["repo","path"]
							}
						}
					},
					"required":["files"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetFileContent,
				Description: "Read the content of a file from a GitHub repository.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"path":{"type":"string","description":"File path within the repository"},
						"branch":{"type":"string","description":"Branch name (optional, uses default branch if empty)"}
					},
					"required":["repo","path"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetRepoDefaultBranch,
				Description: "Get the default branch name of a repository.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"}
					},
					"required":["repo"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetAuthenticatedUser,
				Description: "Get the GitHub username of the authenticated bot user.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolResolveOwner,
				Description: "Resolve the GitHub organization or user that owns repositories.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolFetchChannelContext,
				Description: "Fetch recent messages from the current Slack channel for additional context about the ongoing conversation.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolSearchFiles,
				Description: "Search for files in a repository by name or path pattern. Returns all file paths containing the search term. Use this FIRST when looking for a specific file — it is much faster than navigating directories one by one.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"pattern":{"type":"string","description":"Search term to match against file paths (case-insensitive, e.g. 'services.yaml' or 'amplify/main.tf')"},
						"branch":{"type":"string","description":"Branch name (optional, uses default branch if empty)"}
					},
					"required":["repo","pattern"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolListDirectory,
				Description: "List the files and subdirectories at a path in a GitHub repository. Use this when get_file_content fails because a path is a directory, or when you need to discover what files exist under a path.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"path":{"type":"string","description":"Directory path within the repository"},
						"branch":{"type":"string","description":"Branch name (optional, uses default branch if empty)"}
					},
					"required":["repo","path"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolModifyFile,
				Description: "Modify a file in a GitHub repository using a safe find-and-replace approach. Provide the exact text to find (old_content) and the replacement text (new_content). The tool reads the FULL file from GitHub, performs the replacement, then creates a branch, commits, and opens a PR. Multiple modify_file calls for the SAME repository are automatically grouped into a SINGLE pull request — so when implementing a change that touches several files, just call modify_file for each file and all changes will land in one PR. IMPORTANT: old_content must be an exact substring of the current file — include enough surrounding lines (3-5) will ensure a unique match. Only the matched section is replaced; the rest of the file is preserved.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"path":{"type":"string","description":"File path within the repository"},
						"old_content":{"type":"string","description":"The exact text in the current file to find and replace. Include 3-5 surrounding context lines to ensure a unique match."},
						"new_content":{"type":"string","description":"The replacement text that will replace old_content."},
						"description":{"type":"string","description":"Short description of what was changed (used as commit message and as the PR title when pr_title is not provided)"},
						"pr_body":{"type":"string","description":"OPTIONAL full Markdown body for the pull request. Use this to give reviewers real context (Jira/ticket link, summary of the change, testing notes, applicable skills). Only the FIRST modify_file/create_file/regex_replace_file call per repo per tick establishes the PR body — subsequent calls that group into the same PR are ignored. When omitted, a short generic attribution line is used."},
						"pr_title":{"type":"string","description":"OPTIONAL full PR title. When provided, used VERBATIM as the PR title (no agent-name prefix is added). Only honored on the FIRST write call per repo per tick (the one that opens the PR); subsequent grouped calls are ignored. Leave empty to use the default '<agent-id>: <description>' title."},
						"branch_name":{"type":"string","description":"OPTIONAL custom HEAD branch name to create for the PR (e.g. 'ENG-33153/ovad-fix-foo'). Only honored on the FIRST write call per repo per tick (the one that creates the branch); subsequent grouped calls reuse the already-created branch. Leave empty to use the platform's auto-generated unique branch name. Must be a valid git branch name and must not already exist on the remote."},
						"branch":{"type":"string","description":"BASE branch the PR should be opened against (typically the repo's default branch — main/master). LEAVE EMPTY in almost all cases; the platform auto-resolves the default branch AND auto-generates a unique head branch per run. Only set this if you specifically need to target a long-lived non-default base like 'develop' or 'release/*'. Never pass a head branch from list_pull_requests or a prior PR — those are auto-generated per-tick and will be ignored."}
					},
					"required":["repo","path","old_content","new_content","description"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolCreateFile,
				Description: "Create a NEW file in a GitHub repository. Use this when you need to add a file that does not yet exist (e.g. a new YAML config, a new workflow, a new script). The tool creates a branch, commits the new file, and opens a PR. Multiple create_file and modify_file calls for the SAME repository are automatically grouped into a SINGLE pull request. Do NOT use this for files that already exist — use modify_file instead.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"path":{"type":"string","description":"File path to create within the repository (e.g. 'maintenance/db-maintenance.yaml', '.github/workflows/db-maintenance.yml')"},
						"content":{"type":"string","description":"The full content of the new file."},
						"description":{"type":"string","description":"Short description of what was added (used as commit message and as the PR title when pr_title is not provided)"},
						"pr_body":{"type":"string","description":"OPTIONAL full Markdown body for the pull request. Use this to give reviewers real context (Jira/ticket link, summary, testing notes). Only the FIRST write call per repo per tick establishes the PR body; subsequent calls that group into the same PR are ignored."},
						"pr_title":{"type":"string","description":"OPTIONAL full PR title. When provided, used VERBATIM as the PR title (no agent-name prefix is added). Only honored on the FIRST write call per repo per tick. Leave empty to use the default '<agent-id>: <description>' title."},
						"branch_name":{"type":"string","description":"OPTIONAL custom HEAD branch name to create for the PR. Only honored on the FIRST write call per repo per tick (the one that creates the branch). Leave empty to use the platform's auto-generated unique branch name. Must be a valid git branch name and must not already exist on the remote."},
						"branch":{"type":"string","description":"BASE branch the PR should target. LEAVE EMPTY in almost all cases — the platform resolves the repo default branch and auto-generates the head branch. Do not pass an existing PR head branch here."}
					},
					"required":["repo","path","content","description"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolRegexReplaceFile,
				Description: "Bulk-replace all matches of a SIMPLE regex pattern in a file. Best for uniform single-line replacements across a whole file (e.g. 'change all image.tag to latest'). Do NOT use for scoped/structural changes in a specific section — use modify_file instead for those. Keep patterns short and per-line. Avoid complex multi-line regex with (?:.|\\n)*? or lookaheads — if you need those, use modify_file. The tool reads the FULL file from GitHub, applies a Go RE2 regex replacement on ALL matches, creates a branch, commits, and opens a PR. Multiple calls for the same repo are grouped into a SINGLE PR. Replacement supports $1, $2 for captured groups.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"path":{"type":"string","description":"File path within the repository"},
						"pattern":{"type":"string","description":"Go (RE2) regular expression to match. Use capturing groups for partial replacements (e.g. '(tag:\\s*)\\S+' to capture the 'tag: ' prefix)."},
						"replacement":{"type":"string","description":"Replacement string. Use $1, $2, etc. to reference captured groups (e.g. '${1}latest')."},
						"description":{"type":"string","description":"Short description of what was changed (used as commit message and as the PR title when pr_title is not provided)"},
						"pr_body":{"type":"string","description":"OPTIONAL full Markdown body for the pull request. Use this to give reviewers real context. Only the FIRST write call per repo per tick establishes the PR body; subsequent calls that group into the same PR are ignored."},
						"pr_title":{"type":"string","description":"OPTIONAL full PR title. When provided, used VERBATIM as the PR title (no agent-name prefix is added). Only honored on the FIRST write call per repo per tick. Leave empty to use the default '<agent-id>: <description>' title."},
						"branch_name":{"type":"string","description":"OPTIONAL custom HEAD branch name to create for the PR. Only honored on the FIRST write call per repo per tick (the one that creates the branch). Leave empty to use the platform's auto-generated unique branch name. Must be a valid git branch name and must not already exist on the remote."},
						"branch":{"type":"string","description":"BASE branch the PR should target. LEAVE EMPTY in almost all cases — the platform resolves the repo default branch and auto-generates the head branch. Do not pass an existing PR head branch here."}
					},
					"required":["repo","path","pattern","replacement","description"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetPullRequest,
				Description: "Get details, changed files, and diff of a GitHub pull request by number or URL. Use this to analyze what a PR changed, understand code patterns introduced or removed, and find old/new usage patterns.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"number":{"type":"integer","description":"Pull request number (e.g., 123)"},
						"url":{"type":"string","description":"Full GitHub PR URL (alternative to repo+number). If provided, repo and number are extracted from it."}
					},
					"required":[]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolListPullRequests,
				Description: "List recent pull requests in a repository. Useful for finding relevant PRs by title, discovering recent changes, or identifying the PR that introduced a particular change.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"state":{"type":"string","description":"Filter by state: 'open', 'closed', or 'all' (default: 'all')"},
						"limit":{"type":"integer","description":"Maximum number of PRs to return (default: 10, max: 200)"}
					},
					"required":["repo"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolListCommits,
				Description: "List commits in a repository, optionally filtered by branch, path, author, and a time window. Use this for daily/weekly activity digests (pass since=YYYY-MM-DDT00:00:00Z and until=YYYY-MM-DDT23:59:59Z for one UTC day), or to find the latest/most recent commit that touched a specific file or directory (pass 'path' and limit=1). Returns the full 40-char SHA, first line of commit message, author login, author date, and commit URL. If 'branch' is omitted, the repo's default branch is used — so this captures direct-to-default pushes that never went through a PR, in addition to merge/squash commits from merged PRs. This uses the authenticated GitHub integration, so it works on private repositories (unlike http_get).",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)."},
						"branch":{"type":"string","description":"Branch, tag, or SHA to list commits from. Defaults to the repo's default branch."},
						"path":{"type":"string","description":"Only return commits that touched this file or directory path (e.g. 'src/api' or 'config/settings.yaml'). Combine with limit=1 to get the latest commit affecting that path."},
						"author":{"type":"string","description":"GitHub login or email to filter by author."},
						"since":{"type":"string","description":"ISO-8601 timestamp (e.g. '2026-04-21T00:00:00Z'). Only commits on or after this moment are returned."},
						"until":{"type":"string","description":"ISO-8601 timestamp. Only commits on or before this moment are returned."},
						"limit":{"type":"integer","description":"Maximum number of commits to return (default 30, max 300)."}
					},
					"required":["repo"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolSearchCode,
				Description: "Search for code content within a GitHub repository. Unlike search_files (which matches file names/paths), this searches inside file contents. Use this to find usages of functions, classes, patterns, imports, or any code string across the entire repository. Returns matching files with code fragments showing the context around each match.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"query":{"type":"string","description":"Code search query. Can include the code pattern to find (e.g., 'db.session', 'SessionLocal()', 'def create_session'). Supports GitHub code search qualifiers like 'language:python', 'path:src/', 'extension:py'."}
					},
					"required":["repo","query"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolSearchCodeOrg,
				Description: "Search for code content across ALL repositories in the GitHub organization in a single call. Use this instead of search_code when the user asks to find usages, secrets, patterns, or code across multiple repos, the entire org, or 'every repo'. This is MUCH faster and more complete than calling search_code repo by repo. Returns matching files grouped by repository with code fragments.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"query":{"type":"string","description":"Code search query. The pattern to find across all org repos (e.g., 'clickhouse_connection', 'get_aws_secret'). Supports GitHub code search qualifiers like 'language:python', 'path:src/', 'extension:py'."}
					},
					"required":["query"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetWorkflowRun,
				Description: "Fetch details and logs for a GitHub Actions workflow run. Use this PROACTIVELY whenever you see a failed CI/CD notification, a GitHub Actions URL, or the user mentions a build/deploy/pipeline failure. Returns the run status, jobs, steps, annotations, and actual log output for any failed jobs so you can diagnose the root cause.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"url":{"type":"string","description":"Full GitHub Actions workflow run URL (e.g., 'https://github.com/org/repo/actions/runs/12345'). Extract this from channel context messages — look for 'View Workflow Run' button URLs or similar links."}
					},
					"required":["url"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolRerunFailedJobs,
				Description: "Re-run only the failed jobs (and their dependent jobs) in a GitHub Actions workflow run. This is equivalent to clicking 'Re-run failed jobs' in the GitHub Actions UI. Use this when the user asks to retry, rerun, or re-trigger a failed workflow. Only works on completed runs that have at least one failed job.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"url":{"type":"string","description":"Full GitHub Actions workflow run URL (e.g., 'https://github.com/org/repo/actions/runs/12345')."}
					},
					"required":["url"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolRerunWorkflow,
				Description: "Re-run an entire GitHub Actions workflow run (all jobs, not just failed ones). This is equivalent to clicking 'Re-run all jobs' in the GitHub Actions UI. Use this when the user wants to completely re-trigger a workflow from scratch.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"url":{"type":"string","description":"Full GitHub Actions workflow run URL (e.g., 'https://github.com/org/repo/actions/runs/12345')."}
					},
					"required":["url"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolReplyInThread,
				Description: "Post a message as a threaded reply to a specific Slack message. Use this when the user asks you to reply inside someone's thread or respond to a particular message. You need the thread_ts of the target message from the channel context. IMPORTANT: Messages marked [BOT] are this bot's own messages — never reply to those. Always use the thread_ts of the HUMAN user's message (e.g. the person mentioned by name like 'Alex', 'Sam', etc.).",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"thread_ts":{"type":"string","description":"The thread_ts timestamp of the target human user's message to reply to. MUST be from a non-[BOT] message. Get this from the channel context."},
						"text":{"type":"string","description":"The message text to post as a threaded reply. Supports Slack markdown formatting."}
					},
					"required":["thread_ts","text"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolPostSlackMessage,
				Description: "Post a message to a specific Slack channel by channel ID. Use this when the user gives you an explicit channel ID (e.g. 'C0123456789') and asks you to send a message there, OR inside a scheduled workflow tick where no interactive thread is available. If 'thread_ts' is set, the message is posted as a threaded reply under that parent message (use this to keep multi-part reports on the same thread — capture the ts returned by the first call and pass it as thread_ts on subsequent calls). Supports Slack markdown. Returns the posted message ts on success.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"channel_id":{"type":"string","description":"Slack channel ID (e.g. 'C0123456789'). NOT a channel name."},
						"text":{"type":"string","description":"Message body. Supports Slack markdown formatting."},
						"thread_ts":{"type":"string","description":"OPTIONAL. Parent message ts to reply under. Omit or leave empty to post as a new top-level channel message. Use the ts returned by a prior post_slack_message call to keep a multi-part report threaded together."}
					},
					"required":["channel_id","text"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolSlackConversationsHistory,
				Description: "Read recent messages from a Slack channel by channel ID. Use this in scheduled workflows that need to react to messages posted in a specific channel (e.g. polling an alerts channel). Returns messages newest-first, each annotated with ts, thread_ts, sender, and whether it was posted by a bot. Pass 'oldest' to fetch only messages posted after a prior watermark ts — leave empty to fetch the most recent 'limit' messages regardless of age. Typical first-run usage: oldest='' and limit=1 to grab only the latest message; subsequent ticks: oldest=<previous-watermark-ts> and limit=20.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"channel_id":{"type":"string","description":"Slack channel ID (e.g. 'C0123456789'). NOT a channel name."},
						"oldest":{"type":"string","description":"Slack ts (e.g. '1776940344.123456') — only return messages strictly newer than this. Leave empty or omit to return the most recent 'limit' messages regardless of age."},
						"limit":{"type":"integer","description":"Maximum number of messages to return (default 20, max 100)."}
					},
					"required":["channel_id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolSlackConversationsReplies,
				Description: "Read the replies in a Slack thread by channel ID and the parent message's ts. Use this in scheduled workflows to inspect a thread before acting on it — e.g. to dedupe (check whether a prior tick already replied to an alert with a tracking link or an ack) or to read follow-up context. Returns the parent message followed by its replies, oldest-first, each annotated with ts, thread_ts, sender, and whether it was posted by a bot. The 'thread_ts' is the ts of the parent (top-level) message; for a reply's ts use its 'thread_ts' field to address the parent thread.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"channel_id":{"type":"string","description":"Slack channel ID (e.g. 'C0123456789'). NOT a channel name."},
						"thread_ts":{"type":"string","description":"The ts of the thread's PARENT message (e.g. '1776940344.123456'). This is the top-level message whose replies you want."},
						"limit":{"type":"integer","description":"Maximum number of messages to return, including the parent (default 50, max 200)."}
					},
					"required":["channel_id","thread_ts"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolUploadSnippet,
				Description: "Upload a text file snippet to Slack. Use this instead of posting a long message when the content is large (e.g., full search results with file paths and code lines, log dumps, CSV data, large code blocks). The snippet appears as a collapsible file attachment in the channel/thread, keeping the conversation clean. PREFER this over reply_in_thread or plain messages whenever the output would exceed ~30 lines or ~2000 characters. Ideal for: org-wide search results, full file listings, detailed tables, log output, code dumps. IMPORTANT: you must supply the COMPLETE file body in the 'content' argument of THIS call — it is not generated or filled in for you. Do not call this tool with an empty or omitted 'content'.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"content":{"type":"string","description":"REQUIRED. The full text body of the file, inline as a string. This is the actual file contents (e.g. the entire CSV including header and every data row). Must be non-empty — never omit it or leave it blank."},
						"filename":{"type":"string","description":"Filename for the snippet (e.g. 'search-results.txt', 'clickhouse-usages.csv', 'report.md'). Use an appropriate extension."},
						"title":{"type":"string","description":"Display title for the snippet in Slack (e.g. 'clickhouse_connection usages across org')."},
						"filetype":{"type":"string","description":"Slack file type for syntax highlighting (e.g. 'text', 'markdown', 'csv', 'python', 'yaml', 'json'). Default: 'text'."},
						"channel_id":{"type":"string","description":"Slack channel ID to share the file into. REQUIRED in scheduled workflows (there is no current channel) — use the same channel you post the report to. In an interactive thread it defaults to the current channel."},
						"thread_ts":{"type":"string","description":"OPTIONAL. Parent message ts to attach the file under (e.g. the ts returned by a prior post_slack_message), so the file lands in that thread instead of as a new top-level message."}
					},
					"required":["content","filename","title"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolUploadAggregateCSV,
				Description: "Attach the FULL result(s) of one or more datadog_logs_aggregate calls to Slack as a single downloadable CSV file. Use this for performance-baseline / per-endpoint / per-tenant reports instead of pasting big tables into messages or trying to retype the data into upload_snippet. Each datadog_logs_aggregate call returns an aggregate_id (e.g. 'agg_1'); pass those ids here and the complete tables are rendered server-side — you do NOT need to (and must not) re-emit the rows yourself. The CSV columns are: site,group_by,key,measure,count,p50,p95,p99. The file is posted into the current thread/channel. Optionally pass 's3_bucket' + 's3_key' to ALSO archive the exact same CSV bytes to S3 (durable storage outside Slack's ~retention); on success the tool result includes the s3:// URI and a clickable AWS-console link you can post into the thread.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"aggregate_ids":{"type":"array","items":{"type":"string"},"description":"The aggregate_id values returned by prior datadog_logs_aggregate calls in this run (e.g. [\"agg_1\",\"agg_2\",\"agg_3\"]). All listed results are combined into one CSV, one row per (site, group key)."},
						"filename":{"type":"string","description":"Filename for the CSV (e.g. 'latency-baseline.csv'). Should end in .csv."},
						"title":{"type":"string","description":"Display title for the file in Slack."},
						"channel_id":{"type":"string","description":"Slack channel ID to share the CSV into. REQUIRED in scheduled workflows (there is no current channel) — use the same channel you post the report to. In an interactive thread it defaults to the current channel."},
						"thread_ts":{"type":"string","description":"OPTIONAL. Parent message ts to attach the CSV under (e.g. the ts returned by the summary post_slack_message), so the file lands in that thread."},
						"s3_bucket":{"type":"string","description":"OPTIONAL. When set, the identical file content is also written to this S3 bucket (in addition to the Slack upload). Requires the AWS integration. Pass 's3_key' too. May be a bare bucket name or a full s3://bucket/key URI / arn:aws:s3:::bucket/key ARN."},
						"s3_key":{"type":"string","description":"OPTIONAL. Object key for the S3 archive, e.g. 'reports/2026-06-14/data.csv'. Use whatever folder prefix, filename and extension fit the content. Required when 's3_bucket' is a bare bucket name (omit only if the full key is already embedded in 's3_bucket')."}
					},
					"required":["aggregate_ids","filename","title"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolFetchThreadContext,
				Description: "Fetch the full conversation from a Slack thread URL. Use this FIRST whenever the user provides a Slack thread/message link (https://...slack.com/archives/...) to read the thread's content before acting on it (e.g., creating a Jira ticket, summarizing, replying). Returns all messages in the thread. The response also includes the channel_id and thread_ts so you can reply_in_thread afterwards.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"url":{"type":"string","description":"Slack thread or message URL (e.g. 'https://yourorg.slack.com/archives/C0123456789/p1771847194296799')"}
					},
					"required":["url"]
				}`),
			},
		},
	}

	// NVD CVE lookup tools — gated to agents whose UI card includes NVD.
	if h.canUseIntegration(integrationNVD) && h.nvdClient != nil {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolLookupCVE,
				Description: "Look up a specific CVE by its ID from the NVD (National Vulnerability Database). Returns full details: description, CVSS scores, affected products (CPEs), weaknesses (CWEs), and references. ALWAYS call this tool FIRST when the user mentions a CVE ID (e.g. CVE-2025-13836) to get authoritative data before searching code.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"cve_id":{"type":"string","description":"The CVE identifier (e.g. 'CVE-2025-13836')"}
					},
					"required":["cve_id"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolSearchCVE,
				Description: "Search NVD for CVEs by keyword. Returns matching CVEs with their descriptions and CVSS scores. Useful for finding CVEs related to a specific library, product, or vulnerability type when you don't have the exact CVE ID.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"keyword":{"type":"string","description":"Search keyword(s) to match against CVE descriptions (e.g. 'log4j remote code execution', 'jackson-databind')"},
						"results_per_page":{"type":"integer","description":"Number of results to return (default: 5, max: 20)"}
					},
					"required":["keyword"]
				}`),
			},
		})
	}

	// Salesforce tools — gated to agents whose UI card includes Salesforce.
	if h.canUseIntegration(integrationSalesforce) && h.sfClient != nil && h.sfClient.Ready() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolSalesforceQuery,
				Description: "Execute a SOQL query against Salesforce. Use this to query Accounts, Opportunities, Contacts, and other Salesforce objects. Returns structured records. SOQL is similar to SQL but queries Salesforce objects. IMPORTANT: Use relationship fields for joins (e.g. Account.Name on Opportunity, Owner.Name). Date literals like NEXT_N_DAYS:90, LAST_N_DAYS:30, TODAY, THIS_QUARTER are very useful. Always include Id in SELECT. Limit results to avoid large payloads (default LIMIT 50). Example queries:\n- Renewals in next 90 days: SELECT Id, Name, Account.Name, StageName, CloseDate, Amount, Owner.Name FROM Opportunity WHERE CloseDate = NEXT_N_DAYS:90 AND StageName != 'Closed Won' AND StageName != 'Closed Lost' AND Type = 'Renewal' ORDER BY CloseDate ASC\n- Account details: SELECT Id, Name, Type, Industry, Owner.Name, Website FROM Account WHERE Name LIKE '%CustomerName%'\n- Contacts for account: SELECT Id, Name, Email, Title, Phone, Account.Name FROM Contact WHERE Account.Name LIKE '%CustomerName%'",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"soql":{"type":"string","description":"SOQL query string. Must include Id in SELECT. Use relationship fields for joins (e.g. Account.Name). Use date literals like NEXT_N_DAYS:90, TODAY. Add LIMIT clause to control result size."}
					},
					"required":["soql"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolSalesforceDescribe,
				Description: "Describe a Salesforce object to discover its available fields, types, and labels. Use this when you need to explore object schema before writing a SOQL query — e.g., to find the correct field name for renewal date, health score, or custom fields. Common objects: Account, Opportunity, Contact, Case, Lead, Task, Event.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"object_name":{"type":"string","description":"Salesforce object API name (e.g. 'Account', 'Opportunity', 'Contact', 'Case')"}
					},
					"required":["object_name"]
				}`),
			},
		})
	}

	// Chorus tools — gated to agents whose UI card includes Chorus.
	if h.canUseIntegration(integrationChorus) && h.chorusClient != nil && h.chorusClient.Ready() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolChorusListConversations,
				Description: "List Chorus conversations (meetings, calls, recordings, emails) matching optional filters. This is the primary tool for ANY Chorus query — listings, deal data, engagement activity, participant searches, etc. Returns summaries with meeting notes, action items, participants, trackers, and opportunity info. Dates use ISO-8601 (e.g. '2026-02-01T00:00:00Z'). Optimize which params you pass based on the user's request.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"min_date":{"type":"string","description":"Start of date range (ISO-8601)"},
						"max_date":{"type":"string","description":"End of date range (ISO-8601)"},
						"min_duration":{"type":"number","description":"Minimum meeting duration in seconds"},
						"max_duration":{"type":"number","description":"Maximum meeting duration in seconds"},
						"engagement_type":{"type":"string","description":"Type of engagement (e.g. 'dialer')"},
						"content_type":{"type":"string","description":"Content type filter (e.g. 'email_opened')"},
						"participants_email":{"type":"string","description":"Filter by participant email address"},
						"user_id":{"type":"string","description":"Comma-separated owner User ID(s)"},
						"team_id":{"type":"string","description":"Comma-separated Team ID(s) for engagement owner"},
						"engagement_id":{"type":"string","description":"Comma-separated engagement IDs to fetch specific records"},
						"with_trackers":{"type":"boolean","description":"Return tracker information with results (default false)"},
						"disposition_connected":{"type":"boolean","description":"Filter for connected calls only"},
						"disposition_voicemail":{"type":"boolean","description":"Filter for voicemail calls only"}
					}
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolChorusGetConversation,
				Description: "Get full details for a specific Chorus conversation by its ID. Returns recording analytics, deal info, account, participants with roles/titles, trackers, action items, metrics, and summary. Use this after chorus_list_conversations to drill into a specific call.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"conversation_id":{"type":"string","description":"Chorus conversation ID (from chorus_list_conversations results)"}
					},
					"required":["conversation_id"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolChorusCreateSalesQualification,
				Description: "Extract Sales Qualification Framework data (e.g. MEDDIC) from a Chorus call transcript by recording ID. This triggers AI analysis of the recording and returns structured qualification fields with supporting quotes. Use this when the user wants to analyze a call for sales qualification criteria.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"recording_id":{"type":"string","description":"Chorus recording/conversation ID to analyze"}
					},
					"required":["recording_id"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolChorusGetSalesQualification,
				Description: "Retrieve a previously extracted Sales Qualification Framework analysis by recording ID. Returns MEDDIC (or other framework) fields, supporting quotes, meeting notes, and next steps. Use this to check if a qualification analysis already exists before creating a new one.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"recording_id":{"type":"string","description":"Chorus recording/conversation ID"}
					},
					"required":["recording_id"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolChorusWritebackCRM,
				Description: "Write sales-qualification-derived field updates back to the CRM (e.g. Salesforce). Updates opportunity fields based on insights extracted from Chorus call analysis. Use this after reviewing sales qualification data to push updates to CRM records.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"meeting_id":{"type":"string","description":"Chorus meeting/recording ID that the CRM changes are derived from"},
						"opportunity_id":{"type":"string","description":"CRM opportunity ID to update"},
						"object_type":{"type":"string","description":"CRM object type (default: Opportunity)","enum":["Opportunity"]},
						"crm_changes":{"type":"array","description":"Array of field changes to write back","items":{"type":"object","properties":{"field_name":{"type":"string"},"new_value":{"type":"string"},"previous_value":{"type":"string"}},"required":["field_name","new_value"]}}
					},
					"required":["meeting_id","opportunity_id","crm_changes"]
				}`),
			},
		})
	}

	// Jira tools — gated to agents whose UI card includes Jira/Confluence.
	if h.canUseIntegration(integrationAtlassian) && h.jiraClient != nil && h.jiraClient.Ready() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolCreateJiraTicket,
				Description: "Create a Jira ticket (issue). Use this when the user asks to create a ticket, task, story, or bug from the conversation content (e.g., a test plan, action item, or bug report). Populate the summary and description from the relevant content discussed in the conversation. IMPORTANT: Format the description using markdown — use # for headers, - for bullet lists, 1) for numbered lists, **bold** for emphasis, and `code` for inline code. Structure the ticket professionally with clear sections (e.g., ## Context, ## Scope, ## Acceptance Criteria). If the user asks to assign the ticket to a person, use the assignee field — or pass account_id directly when you already have the Jira accountId from resolve_jira_user (skips the fuzzy name search). If the user asks to assign to a team, use the team field. Set use_active_sprint=true to drop the new ticket into the project's currently active sprint. Use fields for arbitrary Jira fields like priority or custom fields (e.g. {\"priority\":{\"name\":\"High\"}}); typed parameters above always win on conflict.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"summary":{"type":"string","description":"Short one-line title for the ticket."},
						"description":{"type":"string","description":"Detailed, well-structured description using markdown formatting. Use ## for section headers, - for bullet points, 1) for numbered steps, **bold** for key terms, and backticks for code references. Organize into clear sections like Context, Scope, Test Plan, Acceptance Criteria, References, etc."},
						"issue_type":{"type":"string","description":"Issue type: 'Task', 'Bug', 'Story', 'Epic', etc. Default: 'Task'."},
						"labels":{"type":"array","items":{"type":"string"},"description":"Optional labels to apply to the ticket (e.g. ['qa','automated-test'])."},
						"assignee":{"type":"string","description":"Name of the person to assign the ticket to (e.g. 'Ada', 'John Smith'). The system will search for a matching Jira user. Ignored when account_id is set."},
						"account_id":{"type":"string","description":"Jira accountId of the assignee (e.g. '712020:abc-def'). When set, skips the fuzzy name search performed for 'assignee'. Get one via resolve_jira_user."},
						"team":{"type":"string","description":"Name of the team to assign the ticket to (e.g. 'Application', 'DevOps', 'asgard'). The system will search for a matching Jira team."},
						"use_active_sprint":{"type":"boolean","description":"When true, drop the new ticket into the project's currently active sprint (if a scrum board with an active sprint exists). Silently skipped when no active sprint is found."},
						"fields":{"type":"object","additionalProperties":true,"description":"Arbitrary Jira fields to set at create time (e.g. {\"priority\":{\"name\":\"High\"}}, {\"components\":[{\"name\":\"API\"}]}, custom fields like {\"customfield_10010\":\"value\"}). Typed parameters above (summary, description, issue_type, labels, assignee/account_id) always win on conflict."}
					},
					"required":["summary","description"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolListJiraProjects,
				Description: "List all Jira projects visible to the bot. Use this to discover available project keys before creating a ticket.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolSearchJiraIssues,
				Description: "Search for Jira issues using JQL (Jira Query Language). IMPORTANT: Jira Cloud does NOT reliably support searching by display name. Before searching by assignee, you MUST first call resolve_jira_user to get the user's Jira account ID, then use that account ID in JQL (e.g. assignee = 'accountId'). Common JQL examples: 'assignee = \"712020:abc-def\" AND status = \"In Progress\"', 'project = ENG AND status = \"To Do\"'. When searching for a specific user's tickets: 1) call get_slack_user_info to get their real name, 2) call resolve_jira_user with that name to get the Jira account ID, 3) use the account ID in the JQL query.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"jql":{"type":"string","description":"JQL query string (e.g. 'assignee = \"John Doe\" AND status = \"In Progress\" ORDER BY updated DESC')"},
						"max_results":{"type":"integer","description":"Maximum number of results to return (default: 20, max: 50)"}
					},
					"required":["jql"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetJiraIssue,
				Description: "Get full details of a specific Jira issue by its key (e.g. 'ENG-123'). Returns summary, description, status, assignee, priority, labels, and more.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"issue_key":{"type":"string","description":"Jira issue key (e.g. 'ENG-123', 'PROJ-456')"}
					},
					"required":["issue_key"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetJiraLabelAuthor,
				Description: "Determine WHO added (or removed) a label on a Jira issue, read deterministically from the issue changelog/history. This is the ONLY reliable way to attribute a label to a person — the issue's reporter and assignee do NOT reveal who applied a label. Returns each label change in chronological order with the Jira user who made it (display name, account ID, email), and identifies the most recent person who added the label. Use this before stating 'label added by <person>' in any summary.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"issue_key":{"type":"string","description":"Jira issue key (e.g. 'ENG-123', 'PROJ-456')"},
						"label":{"type":"string","description":"The exact label to attribute (e.g. 'arbetern'). Omit to list changes for all labels."}
					},
					"required":["issue_key"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolUpdateJiraIssue,
				Description: "Update a Jira issue's description or summary. Use this to rewrite, refine, or improve ticket descriptions. IMPORTANT: Format the new description using markdown — use # for headers, - for bullet lists, 1) for numbered lists, **bold** for emphasis. Structure it professionally with clear sections.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"issue_key":{"type":"string","description":"Jira issue key (e.g. 'ENG-123')"},
						"summary":{"type":"string","description":"New summary/title for the ticket (optional — only set if you want to change it)"},
						"description":{"type":"string","description":"New description for the ticket in markdown format. Structure with clear sections like ## Context, ## Requirements, ## Acceptance Criteria, etc."}
					},
					"required":["issue_key"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAssignJiraActiveSprint,
				Description: "Move an existing Jira issue into the project's currently active sprint, overriding any prior (stale/closed) sprint value. No-op when the issue is already in the active sprint. Returns an informative message when no scrum board or active sprint exists for the project. Sprint field is the only thing mutated — summary, description, status, assignee are untouched.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"issue_key":{"type":"string","description":"Jira issue key (e.g. 'ENG-123')."},
						"project":{"type":"string","description":"Optional project key. Defaults to the issue's project (derived from issue_key prefix) when omitted."}
					},
					"required":["issue_key"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAssignJiraTeam,
				Description: "Assign an existing Jira issue to a team by team UUID. The Jira Teams integration uses UUIDs (NOT display names) — call resolve_jira_team first to obtain the UUID. Team field is the only thing mutated — summary, description, status, assignee, sprint are untouched. No-op (returns success) when the team is already set to the requested UUID.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"issue_key":{"type":"string","description":"Jira issue key (e.g. 'ENG-123')."},
						"team_id":{"type":"string","description":"Team UUID (from resolve_jira_team). Display names are NOT accepted."}
					},
					"required":["issue_key","team_id"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAddJiraComment,
				Description: "Post a comment on a Jira issue. The body is rendered from markdown to ADF (Atlassian Document Format) automatically — supports # headers, - bullets, 1) ordered lists, **bold**, `code`, and [text](url) links. To @-mention a Jira user so they get a real notification (not just a plain-text string), use the syntax `@[Display Name](accountId)` — e.g. `@[Jane Doe](712020:abc-def-...)`. ALWAYS call resolve_jira_user first to get the accountId; never invent or guess it, and never write a bare `@Name` because it will be posted as inert plain text. Use when the user asks to comment on, reply to, or annotate a ticket.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"issue_key":{"type":"string","description":"Jira issue key (e.g. 'ENG-123')."},
						"body":{"type":"string","description":"Comment body in markdown. Will be converted to ADF before posting."}
					},
					"required":["issue_key","body"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolListJiraComments,
				Description: "List the most recent comments on a Jira issue, newest first. Use to summarize discussion on a ticket or pull recent context before replying.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"issue_key":{"type":"string","description":"Jira issue key (e.g. 'ENG-123')."},
						"limit":{"type":"integer","description":"Max comments to return (default 20, max 100)."}
					},
					"required":["issue_key"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolLinkJiraIssues,
				Description: "Create a typed link between two Jira issues. The 'link_type' is the relationship name as it appears in Jira (e.g. 'Relates', 'Blocks', 'Causes', 'Duplicates', 'Cloners'). The inward issue is the source of the relationship and the outward issue is the target — e.g. for 'Blocks', inward_key blocks outward_key.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"inward_key":{"type":"string","description":"Source issue key (e.g. 'ENG-123')."},
						"outward_key":{"type":"string","description":"Target issue key (e.g. 'ENG-456')."},
						"link_type":{"type":"string","description":"Link type as shown in Jira (e.g. 'Relates', 'Blocks', 'Causes', 'Duplicates')."}
					},
					"required":["inward_key","outward_key","link_type"]
				}`),
			},
		})
	}

	// Slack user info tool is always available.
	tools = append(tools, llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        ToolGetSlackUserInfo,
			Description: "Get the real name and profile information of a Slack user by their user ID. Use this to resolve the current user's real name for Jira queries. The user_id is available from the conversation context (the person who sent the command).",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"user_id":{"type":"string","description":"Slack user ID (e.g. 'U01ABC123'). Use the current user's ID from the command context."}
				},
				"required":["user_id"]
			}`),
		},
	})

	// Reverse Slack lookup: resolve a person to their Slack user ID so the model
	// can build a real <@ID> mention. The canonical key is email (Slack
	// users.lookupByEmail); name is a conservative exact-match fallback.
	tools = append(tools, llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        ToolLookupSlackUser,
			Description: "Resolve a person to their Slack user ID so you can @-mention them. This is the ONLY way to turn a Jira user (e.g. the label author from get_jira_label_author) into a real Slack ping. Pass their email (MOST reliable — uses Slack users.lookupByEmail) and/or their display name. Returns the Slack user ID and the ready-to-paste mention token `<@USERID>`. A bare `@Display Name` in a Slack message is inert text that pings nobody — you MUST use the `<@USERID>` form from this tool to actually notify someone. If the person cannot be uniquely resolved, the tool says so; in that case fall back to plain `@Display Name` rather than guessing an ID.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"email":{"type":"string","description":"The person's email address (most reliable). Use the email returned by get_jira_label_author / resolve_jira_user when available."},
					"name":{"type":"string","description":"The person's display/real name (e.g. 'Ada Lovelace'). Used only as a fallback when email is missing or does not match; resolves ONLY when exactly one Slack user has that name."}
				}
			}`),
		},
	})

	// Jira user resolution tool — resolves a person's name/email to their Jira account ID.
	if h.canUseIntegration(integrationAtlassian) && h.jiraClient != nil && h.jiraClient.Ready() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolResolveJiraUser,
				Description: "Search for a Jira user by name and/or email and return their account ID. IMPORTANT: Jira Cloud JQL does NOT reliably support searching by display name (e.g. assignee = 'Jane Doe' may return zero results). You MUST call this tool first to get the user's Jira account ID, then use that account ID in JQL queries (e.g. assignee = 'accountId'). This is the ONLY reliable way to find issues by assignee in Jira Cloud. ALWAYS pass both name AND email (from get_slack_user_info) for best results — email-based search is the most reliable.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"name":{"type":"string","description":"The person's display name (e.g. 'Jane Doe', 'John Smith')"},
						"email":{"type":"string","description":"The person's email address (most reliable for Jira lookup). Get this from get_slack_user_info."}
					},
					"required":["name"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolResolveJiraTeam,
				Description: "Resolve a Jira team name to its UUID and JQL clause name. The Jira Teams integration field uses UUIDs, NOT display names, in JQL. You MUST call this tool first when searching for a team's tickets — it returns the JQL clause (e.g. 'Team[Team]') and team UUID. Then use the result in JQL like: '\"Team[Team]\" = \"<uuid>\"'. Example: resolve_jira_team({\"team_name\": \"DevOps\"}) → clause='Team[Team]', uuid='d6c2ac7c-...', then search with JQL '\"Team[Team]\" = \"d6c2ac7c-...\" AND status = \"In Progress\"'.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"team_name":{"type":"string","description":"The team name to resolve (e.g. 'DevOps', 'Platforms', 'Remediation')"}
					},
					"required":["team_name"]
				}`),
			},
		})

		// Dashboard & filter tools.
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetJiraDashboard,
				Description: "Fetch a Jira dashboard by its numeric ID, including its name, owner, and all configured gadgets. Use this when a user shares a dashboard URL like https://org.atlassian.net/jira/dashboards/10014 — extract the numeric ID from the URL. Returns dashboard metadata and a list of gadgets with their titles and positions.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"dashboard_id":{"type":"string","description":"Numeric dashboard ID (e.g. '10014'). Extract from a dashboard URL: /jira/dashboards/<id>"}
					},
					"required":["dashboard_id"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetJiraFilter,
				Description: "Fetch a Jira saved filter by its numeric ID and optionally execute its JQL to return matching issues. Use this when a user shares a filter URL like https://org.atlassian.net/issues/?filter=11901 — extract the numeric ID. Returns the filter name, owner, JQL query, and (if run_jql is true) the issues matching that JQL.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"filter_id":{"type":"string","description":"Numeric filter ID (e.g. '11901'). Extract from a filter URL: /issues/?filter=<id>"},
						"run_jql":{"type":"boolean","description":"If true, also execute the filter's JQL and return the matching issues (default: true)"},
						"max_results":{"type":"integer","description":"Maximum issues to return when run_jql is true (default: 20, max: 50)"}
					},
					"required":["filter_id"]
				}`),
			},
		})

		// Confluence tools — share the same Atlassian client as Jira.
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolSearchConfluencePages,
				Description: "Search Confluence pages using CQL (Confluence Query Language). Useful for finding documentation, runbooks, architecture decision records, and knowledge-base articles. Common CQL examples: 'text ~ \"deployment guide\"', 'space = DEV AND title ~ \"runbook\"', 'label = \"architecture\" AND type = page'. Returns page IDs, titles, and links — use get_confluence_page to fetch the full content of a matching page.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"cql":{"type":"string","description":"CQL query (e.g. 'text ~ \"kubernetes\" AND space = ENG')"},
						"limit":{"type":"integer","description":"Max results (default 10, max 25)"}
					},
					"required":["cql"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolGetConfluencePage,
				Description: "Retrieve the full content of a Confluence page. Accepts a numeric page ID, a full Confluence page URL, or a Confluence short/tiny link (e.g. https://site.atlassian.net/wiki/x/AQAF3Q). Returns the page title, body (in storage/XHTML format), and a web link. Use search_confluence_pages first if you need to find a page by title or content.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"page_id":{"type":"string","description":"Confluence page ID (numeric), full page URL, or tiny/short link URL (e.g. 'https://site.atlassian.net/wiki/x/AQAF3Q')"}
					},
					"required":["page_id"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolListConfluenceSpaces,
				Description: "List all Confluence spaces visible to the bot. Useful for discovering available spaces before searching for pages. Returns space keys, names, and types.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolCreateConfluencePage,
				Description: "Create a new Confluence page in a specified space. Use this when the user asks to create documentation, write a guide, or generate a wiki page. The body should be in Confluence storage format (XHTML). You can use basic HTML tags: <h1>-<h6> for headings, <p> for paragraphs, <ul>/<ol>/<li> for lists, <table>/<tr>/<th>/<td> for tables, <code> for inline code, <ac:structured-macro ac:name=\"code\"><ac:plain-text-body><![CDATA[...]]></ac:plain-text-body></ac:structured-macro> for code blocks, <strong> for bold, <em> for italic, and <a href=\"url\">text</a> for links.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"space_key":{"type":"string","description":"Confluence space key (e.g. 'DO', 'ENG'). Use list_confluence_spaces to discover available spaces."},
						"title":{"type":"string","description":"Page title."},
						"body":{"type":"string","description":"Page body in Confluence storage format (XHTML). Use HTML tags for formatting."},
						"parent_id":{"type":"string","description":"Optional parent page ID to nest under. Use search_confluence_pages or get_confluence_page to find the parent page ID."}
					},
					"required":["space_key","title","body"]
				}`),
			},
		})
	}

	// Datadog tools — gated to agents whose UI card includes Datadog.
	if h.canUseIntegration(integrationDatadog) && h.datadogClients != nil {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolDatadogSearchLogs,
				Description: "Search Datadog logs using the Log Search API. Use this to find error logs, investigate incidents, debug service issues, or check what happened in a specific time window. Supports full Datadog log query syntax (e.g. 'service:my-service status:error', 'pod_name:my-pod* @error', 'env:prod source:kubernetes'). ALWAYS use this when the user asks about logs, errors, or recent failures in production. Time range defaults to the last 1 hour if not specified.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"query":{"type":"string","description":"Datadog log search query (e.g. 'service:my-service status:error', 'pod_name:my-pod', 'env:prod @http.status_code:>499')"},
						"from":{"type":"string","description":"Start of time range in ISO-8601 (e.g. '2026-03-12T10:00:00Z'). Defaults to 1 hour ago."},
						"to":{"type":"string","description":"End of time range in ISO-8601. Defaults to now."},
						"limit":{"type":"integer","description":"Max log entries to return (default: 20, max: 50)"},
						"site":{"type":"string","enum":["us","eu"],"description":"Datadog site to query: 'us' (datadoghq.com), 'eu' (datadoghq.eu). Infer from URLs in the user's message. Omit to query all configured sites."}
					},
					"required":["query"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolDatadogLogsAggregate,
				Description: "Compute SERVER-SIDE aggregations and accurate percentiles over Datadog logs (no sampling, no row cap — unlike datadog_search_logs). For each bucket of the group-by facet(s) it returns the event count and, when a measure is given, p50/p95/p99 of that numeric measure, sorted by p95 descending (by count when no measure). Use this for performance baselines and 'how slow is X / which values are worst / how many events per group' questions. Example: query 'service:my-service status:ok', group_by '@resource_name', measure '@duration' → per-resource latency percentiles over the whole window. Omit measure for a count-only breakdown (e.g. group_by '@error_id' to get per-error event counts). Pass MULTIPLE facets (e.g. group_by ['@resource_name','@http.method']) to get a composite breakdown — one row per facet-value combination — instead of a single global number. Omit group_by for one overall bucket. Requires any measure to be a registered Datadog measure facet. Time range defaults to the last 14 days.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"query":{"type":"string","description":"Datadog log search query to aggregate over (e.g. 'service:my-service status:ok'). Append exclusions like '-@some_facet:(test* OR demo*)' to drop unwanted values."},
						"group_by":{"description":"Facet(s) to group by. Either a single facet string (e.g. '@resource_name') or an array of facets for a composite breakdown (e.g. ['@resource_name','@http.method']). Omit for a single overall bucket (no grouping).","oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]},
						"measure":{"type":"string","description":"OPTIONAL. Numeric measure facet to compute p50/p95/p99 over (e.g. '@duration', '@network.bytes_written'). Omit for a count-only aggregation (just the event count per bucket). Must be a registered Datadog measure facet when provided."},
						"from":{"type":"string","description":"Start of time range: ISO-8601, unix-ms, or date-math ('now-14d'). Defaults to 14 days ago."},
						"to":{"type":"string","description":"End of time range. Defaults to now."},
						"site":{"type":"string","enum":["us","eu"],"description":"Datadog site to query: 'us' (datadoghq.com), 'eu' (datadoghq.eu). Omit to query all configured sites (recommended for platform-wide baselines)."}
					},
					"required":["query"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolDatadogListMonitors,
				Description: "List Datadog monitors, optionally filtered by query. Use this to find monitors related to a service, environment, or alert type. Supports Datadog monitor search syntax (e.g. 'tag:service:my-service', 'tag:env:prod', monitor name substrings). Returns monitor name, status (OK/Alert/Warn/No Data), type, tags, and direct links.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"query":{"type":"string","description":"Monitor search query (e.g. 'tag:env:prod', 'my-service', 'tag:team:devops'). Empty returns all monitors."},
						"limit":{"type":"integer","description":"Max monitors to return (default: 20, max: 50)"},
						"site":{"type":"string","enum":["us","eu"],"description":"Datadog site to query: 'us' (datadoghq.com), 'eu' (datadoghq.eu). Infer from URLs in the user's message. Omit to query all configured sites."}
					}
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolDatadogGetMonitor,
				Description: "Get full details of a specific Datadog monitor by its numeric ID. Returns the monitor's name, query, message (notification template with reasoning/escalation steps), status, tags, creator, and timestamps. Use this to understand WHY a monitor triggered — the query shows the threshold condition and the message contains the team's documented reasoning and next steps. Extract the monitor ID from Datadog URLs (e.g. https://app.datadoghq.com/monitors/12345 → ID is 12345, https://app.datadoghq.eu/monitors/12345 → ID is 12345, site=eu).",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"monitor_id":{"type":"string","description":"Numeric monitor ID (e.g. '12345'). Extract from Datadog monitor URLs."},
						"site":{"type":"string","enum":["us","eu"],"description":"Datadog site to query: 'us' (datadoghq.com), 'eu' (datadoghq.eu). Infer from URLs in the user's message. Omit to try all configured sites."}
					},
					"required":["monitor_id"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolDatadogListHosts,
				Description: "List infrastructure hosts from Datadog, optionally filtered. Use this to check host health, find hosts running a specific service, or investigate infrastructure issues. Returns host names, up/down status, apps, tags, and cloud metadata.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"filter":{"type":"string","description":"Filter hosts by name, tag, or other attributes (e.g. 'web', 'env:prod')"},
						"count":{"type":"integer","description":"Max hosts to return (default: 20, max: 100)"},
						"site":{"type":"string","enum":["us","eu"],"description":"Datadog site to query: 'us' (datadoghq.com), 'eu' (datadoghq.eu). Infer from URLs in the user's message. Omit to query all configured sites."}
					}
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolDatadogGetDashboard,
				Description: "Fetch a Datadog dashboard by its ID. Returns the dashboard title, description, layout, author, and all widget definitions (titles, types). Use this when a user shares a Datadog dashboard URL (e.g. https://app.datadoghq.com/dashboard/abc-def-ghi or https://app.datadoghq.eu/dashboard/abc-def-ghi) — extract the dashboard ID from the URL path. Useful for understanding what metrics/monitors a team tracks.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"dashboard_id":{"type":"string","description":"Dashboard ID (e.g. 'abc-def-ghi'). Extract from Datadog dashboard URLs: /dashboard/<id>/title"},
						"site":{"type":"string","enum":["us","eu"],"description":"Datadog site to query: 'us' (datadoghq.com), 'eu' (datadoghq.eu). Infer from URLs in the user's message. Omit to try all configured sites."}
					},
					"required":["dashboard_id"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolDatadogListDashboards,
				Description: "List Datadog dashboards, optionally filtered by title keyword. Use this to discover available dashboards for a team, service, or topic.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"query":{"type":"string","description":"Filter dashboards by title keyword (e.g. 'my-service', 'production', 'SLO'). Empty returns all."},
						"count":{"type":"integer","description":"Max dashboards to return (default: 20, max: 50)"},
						"site":{"type":"string","enum":["us","eu"],"description":"Datadog site to query: 'us' (datadoghq.com), 'eu' (datadoghq.eu). Infer from URLs in the user's message. Omit to query all configured sites."}
					}
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolDatadogQueryMetrics,
				Description: "Run a Datadog timeseries metrics query and get per-series aggregates (avg/min/max/last). Use this whenever the user asks about CPU, memory, network, disk, latency, throughput, error rate, or any numeric metric broken down by tags (service, pod, cluster, host, etc.) — this is the ONLY tool that can answer those questions. Returns series sorted by avg descending so low/high utilizers are easy to spot. Do client-side filtering (e.g. \"under 20% CPU\") by reading the series list in the result. Query MUST be a valid Datadog metric expression with an aggregator and scope. Examples: `avg:kubernetes.cpu.usage.total{cluster_name:prod-us} by {kube_service}` (raw CPU usage), `avg:kubernetes.cpu.usage.total{*} by {kube_service} / avg:kubernetes.cpu.limits{*} by {kube_service} * 100` (CPU utilization vs limit, %), `avg:kubernetes.memory.usage{*} by {kube_deployment}`, `sum:kubernetes.pods.running{*} by {cluster_name}`, `avg:system.load.1{*} by {host}`. Prefer `by {kube_service}` or `by {kube_deployment}` when the user asks \"which services/deployments\". Default time window is the last 1 hour. If the initial query returns too many series, re-run with a tighter scope filter.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"query":{"type":"string","description":"Full Datadog metric query expression, e.g. 'avg:kubernetes.cpu.usage.total{cluster_name:prod} by {kube_service}' or 'avg:kubernetes.cpu.usage.total{*} by {kube_service} / avg:kubernetes.cpu.limits{*} by {kube_service} * 100'. Must include an aggregator (avg/sum/min/max), a metric name, a scope filter {...}, and optionally a grouping 'by {tag}'."},
						"from":{"type":"string","description":"Start of the time window. Accepts ISO-8601 ('2026-04-19T10:00:00Z'), unix seconds, or a relative duration ('-1h', '-15m', '-7d'). Defaults to -1h."},
						"to":{"type":"string","description":"End of the time window (ISO-8601, unix seconds, or a duration). Defaults to now."},
						"site":{"type":"string","enum":["us","eu"],"description":"Datadog site to query: 'us' (datadoghq.com), 'eu' (datadoghq.eu). Infer from URLs in the user's message. Omit to query all configured sites."}
					},
					"required":["query"]
				}`),
			},
		})
	}

	// AWS tools — only Cost Explorer for now. Enabled when AWS credentials
	// resolved at startup. All three tools share the single us-east-1-signed
	// Cost Explorer client; cost data returned is account-global. Gated to
	// agents whose UI card includes AWS.
	if h.canUseIntegration(integrationAWS) && h.awsClient != nil {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAWSGetCostAndUsage,
				Description: "Query AWS Cost Explorer for cost and usage data. Use this for daily/weekly/monthly cost reports, cost-by-service breakdowns, cost-by-account (for payer / linked accounts), week-over-week trend analysis, and anomaly spotting. 'start' and 'end' are YYYY-MM-DD and 'end' is EXCLUSIVE (Cost Explorer convention: to report through 2026-04-21 inclusive, pass end=2026-04-22). Default window is the last 8 days at DAILY granularity with AmortizedCost — AmortizedCost is used by default so Reserved Instance and Savings Plan up-front charges are spread evenly across their commitment term (required for accurate daily trend analysis on accounts that use RIs/SPs). Set group_by to break down by SERVICE (e.g. 'Amazon Elastic Compute Cloud - Compute', 'Amazon Relational Database Service', 'AWS Lambda'), LINKED_ACCOUNT, REGION, USAGE_TYPE, INSTANCE_TYPE, OPERATION, PURCHASE_TYPE, RECORD_TYPE. Use service_filter to restrict to one exact service name (find the exact string via aws_list_dimension_values with dimension=SERVICE). Use exclude_charge_types to drop RECORD_TYPE rows that aren't real spend — pass [\"Credit\",\"Refund\",\"Tax\",\"Solution Provider Program Discount\"] to mirror the AWS console's default Charge-type filter so reported totals match the console. Use exclude_services to drop whole services whose name CONTAINS a substring (case-insensitive, e.g. [\"Databricks\"]) from the total, every group, and downstream math — the tool resolves the matching exact service names automatically, so you never need a separate grouped call just to post-filter them out. WARNING: each Cost Explorer API call costs $0.01 — avoid looping over services; prefer one grouped call over N filtered calls.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"start":{"type":"string","description":"Start date YYYY-MM-DD, inclusive. Defaults to 8 days ago."},
						"end":{"type":"string","description":"End date YYYY-MM-DD, EXCLUSIVE. Defaults to today."},
						"granularity":{"type":"string","enum":["DAILY","MONTHLY","HOURLY"],"description":"Granularity. Default DAILY."},
						"metric":{"type":"string","enum":["UnblendedCost","BlendedCost","AmortizedCost","NetAmortizedCost","NetUnblendedCost","UsageQuantity"],"description":"Cost metric. Default AmortizedCost (spreads RI/SP up-front charges across their commitment term). Use UnblendedCost to match the console's default view."},
						"group_by":{"type":"string","enum":["SERVICE","LINKED_ACCOUNT","REGION","USAGE_TYPE","INSTANCE_TYPE","OPERATION","PURCHASE_TYPE","RECORD_TYPE","AVAILABILITY_ZONE","PLATFORM","TENANCY","DATABASE_ENGINE"],"description":"Optional grouping dimension. Omit for a single total per period."},
						"service_filter":{"type":"string","description":"Exact AWS service name to restrict to (case-sensitive, e.g. 'Amazon Elastic Compute Cloud - Compute'). Use aws_list_dimension_values with dimension=SERVICE to discover exact strings."},
						"exclude_charge_types":{"type":"array","items":{"type":"string"},"description":"Optional RECORD_TYPE values to exclude (case-sensitive). Pass [\"Credit\",\"Refund\",\"Tax\",\"Solution Provider Program Discount\"] to match the console's default Charge-type filter."},
						"exclude_services":{"type":"array","items":{"type":"string"},"description":"Optional SERVICE-name substrings to exclude (case-insensitive, e.g. [\"Databricks\"]). Any service whose name contains one of these is dropped from the total and every group; the tool resolves the exact service names for you. Applies whether or not group_by=SERVICE."}
					}
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAWSGetCostForecast,
				Description: "Project future AWS spend using Cost Explorer's forecast model. Use this when the user asks 'how much will we spend next month / this week?'. 'start' must be >= today (CE rejects past dates) and 'end' must be within 12 months. Default window: tomorrow → +30 days at DAILY granularity. Pass exclude_charge_types to mirror the console's default Charge-type filter so the forecast matches the console's projection. Pass exclude_services to project spend with whole services removed (e.g. [\"Databricks\"]); the tool resolves the matching exact names over the last 30 settled days and excludes them from the projection, so you do not need to approximate the exclusion yourself.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"start":{"type":"string","description":"Start date YYYY-MM-DD, inclusive. Must be >= today. Defaults to tomorrow."},
						"end":{"type":"string","description":"End date YYYY-MM-DD, exclusive. Defaults to 30 days from now."},
						"granularity":{"type":"string","enum":["DAILY","MONTHLY"],"description":"Forecast granularity. Default DAILY."},
						"metric":{"type":"string","enum":["UnblendedCost","BlendedCost","AmortizedCost","NetAmortizedCost","NetUnblendedCost","UsageQuantity"],"description":"Cost metric. Default UnblendedCost."},
						"exclude_charge_types":{"type":"array","items":{"type":"string"},"description":"Optional RECORD_TYPE values to exclude (case-sensitive). Pass [\"Credit\",\"Refund\",\"Tax\",\"Solution Provider Program Discount\"] to match the console's default Charge-type filter."},
						"exclude_services":{"type":"array","items":{"type":"string"},"description":"Optional SERVICE-name substrings to exclude from the forecast (case-insensitive, e.g. [\"Databricks\"]). The tool resolves the matching exact service names over the last 30 settled days and removes them from the projection."}
					}
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAWSListDimensionValues,
				Description: "Enumerate possible values for a Cost Explorer dimension (e.g. list every SERVICE that accrued cost in the last 30 days). Useful to discover the exact service name strings before using them as service_filter in aws_get_cost_and_usage — service names are long and finicky ('Amazon Elastic Compute Cloud - Compute', not 'EC2').",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"dimension":{"type":"string","enum":["SERVICE","LINKED_ACCOUNT","REGION","USAGE_TYPE","INSTANCE_TYPE","OPERATION","PURCHASE_TYPE","RECORD_TYPE","AVAILABILITY_ZONE","PLATFORM","TENANCY","DATABASE_ENGINE"],"description":"Dimension to list values for."},
						"start":{"type":"string","description":"Start date YYYY-MM-DD, inclusive. Defaults to 30 days ago."},
						"end":{"type":"string","description":"End date YYYY-MM-DD, exclusive. Defaults to today."},
						"search":{"type":"string","description":"Optional substring filter applied server-side."}
					},
					"required":["dimension"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAWSS3PutObject,
				Description: "Write (upload) text content to an object in an S3 bucket. The object is created or overwritten at the exact key you give (S3 objects are immutable: every write replaces the whole object, so to APPEND you must first read the object with aws_s3_get_object, concatenate your new content, and write the combined text back). 'bucket' is the bucket name — you may instead paste a full 's3://bucket/key' URI or an 'arn:aws:s3:::bucket/key' ARN and leave 'key' empty; the bucket and key are split for you. 'key' is the full object path including any folder prefixes and the filename (e.g. 'reports/2026-06-11.csv'). 'content' is the literal text stored as the object body. The bucket's region is detected automatically.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"bucket":{"type":"string","description":"S3 bucket name, or a full s3://bucket/key URI or arn:aws:s3:::bucket/key ARN (the key is split out automatically when embedded here)."},
						"key":{"type":"string","description":"Full object key/path including prefixes and filename, e.g. 'reports/2026-06-11.csv'. May be omitted only when the full path is already embedded in 'bucket'."},
						"content":{"type":"string","description":"The literal text to store as the object body (e.g. CSV or JSON text). Replaces the object entirely."},
						"content_type":{"type":"string","description":"Optional MIME type. Inferred from the key extension (.csv/.json/.txt/.md) when omitted."}
					},
					"required":["bucket","content"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAWSS3GetObject,
				Description: "Read (download) the text content of an S3 object — for example to read back an object before rewriting it with appended content. Returns the object body plus its size and last-modified time. Bodies larger than 1 MiB are truncated. 'bucket' may be a bare name, an 's3://bucket/key' URI, or an 'arn:aws:s3:::bucket/key' ARN; 'key' is the full object path. An error that the object does not exist (NoSuchKey) is the normal signal that the file has not been created yet — handle it by creating the object with aws_s3_put_object.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"bucket":{"type":"string","description":"S3 bucket name, or a full s3://bucket/key URI or arn:aws:s3:::bucket/key ARN."},
						"key":{"type":"string","description":"Full object key/path, e.g. 'reports/2026-06-11.csv'. May be omitted only when embedded in 'bucket'."}
					},
					"required":["bucket"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAWSS3ListObjects,
				Description: "List objects in an S3 bucket, optionally under a key prefix — for example to enumerate which dated files already exist under a folder. Returns each object's key, size and last-modified time, sorted by key so date-stamped filenames come back in chronological order. One page only, capped at 1000 keys. 'bucket' may be a bare name, an 's3://' URI, or an ARN; 'prefix' narrows the listing.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"bucket":{"type":"string","description":"S3 bucket name, or a full s3://bucket/prefix URI or arn:aws:s3:::bucket/prefix ARN."},
						"prefix":{"type":"string","description":"Optional key prefix to narrow the listing, e.g. 'reports/'. Omit to list from the bucket root."},
						"max_keys":{"type":"integer","description":"Optional maximum number of keys to return (1-1000). Defaults to 1000."}
					},
					"required":["bucket"]
				}`),
			},
		})
	}

	// Azure tools — Cost Management API scoped at the configured
	// management group (defaults to the tenant root MG, i.e. tenant-wide).
	// Enabled when AZURE_TENANT_ID / AZURE_CLIENT_ID /
	// AZURE_CLIENT_SECRET are all present and a service-principal token
	// resolved at startup. Mirrors the AWS tool surface
	// (cost-and-usage, forecast, dimension-values) so workflows can be
	// ported across clouds with minimal prompt changes.
	if h.azureClient != nil {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAzureGetCostAndUsage,
				Description: "Query Azure Cost Management for cost and usage data across every subscription nested under the configured management group (defaults to the tenant root MG — tenant-wide). Use for daily/weekly/monthly cost reports, cost-by-service breakdowns, cost-by-resource-group, region splits, per-subscription breakdowns (group_by=SubscriptionName / SubscriptionId), and trend analysis. 'start' and 'end' are YYYY-MM-DD and 'end' is treated EXCLUSIVE (mirrors AWS Cost Explorer: to report through 2026-04-21 inclusive, pass end=2026-04-22 — Azure's underlying API is inclusive but arbetern normalises). Default window: last 8 days at Daily granularity, ActualCost. Pass AmortizedCost to spread Reservation / Savings Plan up-front charges across their commitment term. Set group_by to break down by ServiceName (e.g. 'Virtual Machines', 'Storage', 'Azure Kubernetes Service'), ResourceGroupName, ResourceLocation, MeterCategory, ChargeType, SubscriptionName, SubscriptionId, ResourceId. Use service_filter to restrict to one exact ServiceName (find the exact string via azure_list_dimension_values with dimension=ServiceName).",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"start":{"type":"string","description":"Start date YYYY-MM-DD, inclusive. Defaults to 8 days ago."},
						"end":{"type":"string","description":"End date YYYY-MM-DD, EXCLUSIVE. Defaults to today."},
						"granularity":{"type":"string","enum":["Daily","Monthly","None"],"description":"Granularity. Default Daily. 'None' returns one bucket for the whole window."},
						"metric":{"type":"string","enum":["ActualCost","AmortizedCost"],"description":"Cost type. Default ActualCost (matches Azure portal default). Use AmortizedCost to spread Reservation / Savings Plan up-front charges across their commitment term."},
						"group_by":{"type":"string","description":"Optional grouping dimension (ServiceName, ResourceGroupName, ResourceLocation, MeterCategory, ChargeType, SubscriptionName, SubscriptionId, ResourceId, ...). Use SubscriptionName / SubscriptionId to break tenant-wide spend down per subscription. Omit for a single total per period."},
						"service_filter":{"type":"string","description":"Exact Azure ServiceName to restrict to (case-sensitive, e.g. 'Virtual Machines'). Use azure_list_dimension_values with dimension=ServiceName to discover exact strings."}
					}
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAzureGetCostForecast,
				Description: "Project future Azure spend across every subscription nested under the configured management group (tenant-wide by default) using Cost Management's forecast endpoint. Use this when the user asks 'how much will we spend next month / this week?'. Default window: today → +30 days at Daily granularity, ActualCost.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"start":{"type":"string","description":"Start date YYYY-MM-DD, inclusive. Defaults to today."},
						"end":{"type":"string","description":"End date YYYY-MM-DD, exclusive. Defaults to 30 days from now."},
						"granularity":{"type":"string","enum":["Daily","Monthly"],"description":"Forecast granularity. Default Daily."},
						"metric":{"type":"string","enum":["ActualCost","AmortizedCost"],"description":"Cost type. Default ActualCost."}
					}
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolAzureListDimensionValues,
				Description: "Enumerate possible values for an Azure Cost Management dimension (e.g. list every ServiceName that accrued cost, or every SubscriptionName the SP can see) across the configured management group. Useful to discover the exact service-name strings before passing them as service_filter to azure_get_cost_and_usage — Azure service names are case-sensitive and finicky ('Virtual Machines', not 'VM'; 'Azure Kubernetes Service', not 'AKS').",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"dimension":{"type":"string","description":"Dimension to list values for. Common: ServiceName, ResourceGroupName, ResourceLocation, MeterCategory, ChargeType, SubscriptionName, SubscriptionId, ResourceId."},
						"search":{"type":"string","description":"Optional substring filter (case-insensitive, client-side)."},
						"top":{"type":"integer","description":"Max values to return (default 100, max 1000)."}
					},
					"required":["dimension"]
				}`),
			},
		})
	}

	// databricks_query: read-only SQL against a Databricks SQL warehouse.
	// Access is gated by the per-agent integration allowlist in helpers.go
	// (restrictedIntegrations) — currently ovad only — AND by the client
	// having completed its first OAuth token exchange, so the tool never
	// advertises itself when the warehouse is unreachable.
	if h.canUseIntegration(integrationDatabricks) && h.databricksClient != nil && h.databricksClient.Ready() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolDatabricksQuery,
				Description: "Run read-only SQL against the configured Databricks SQL warehouse and return the result rows as a table. Use for ad-hoc analytics over Unity Catalog tables and Databricks system tables (e.g. system.billing.usage for cost/DBU analysis), including SELECT, WITH (CTEs), SHOW, DESCRIBE, EXPLAIN and VALUES. You may send a script of several read-only statements separated by semicolons — e.g. one or more DECLARE/SET session variables followed by a final SELECT — and the warehouse returns the result of the LAST statement. EVERY statement must be read-only: INSERT/UPDATE/DELETE/MERGE/CREATE/DROP/ALTER/TRUNCATE/GRANT and other writes are rejected, whether standalone or hidden inside a WITH/BEGIN block. AI/ML SQL functions such as ai_forecast(), ai_query() and vector_search() are supported inside a SELECT/WITH. Still prefer named parameter markers (:name) supplied via the 'parameters' array for user-supplied values — e.g. SELECT * FROM t WHERE day >= :since with parameters [{\"name\":\"since\",\"value\":\"2024-01-01\",\"type\":\"DATE\"}] — since it avoids quoting/injection bugs; use DECLARE/SET only when you genuinely need session variables.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"sql":{"type":"string","description":"Read-only SQL: a single statement, or a script of several read-only statements separated by semicolons (e.g. DECLARE/SET … ; WITH … SELECT …). Only SELECT / WITH / SHOW / DESCRIBE / EXPLAIN / VALUES plus DECLARE / SET / USE for session setup are allowed; mutations are rejected. Prefer :name markers for any user-supplied values and pass them via 'parameters'."},
						"parameters":{
							"type":"array",
							"description":"Named query parameters bound to :name markers in the SQL. Prefer this over string-concatenating values into the SQL (prevents injection and quoting bugs).",
							"items":{
								"type":"object",
								"properties":{
									"name":{"type":"string","description":"Marker name WITHOUT the leading colon (e.g. 'since' binds to :since)."},
									"value":{"type":"string","description":"Parameter value as a string. Databricks casts it per 'type'. Use null only via omission."},
									"type":{"type":"string","description":"Optional SQL type for the bind, e.g. STRING, INT, BIGINT, DOUBLE, DECIMAL(10,2), BOOLEAN, DATE, TIMESTAMP. Defaults to STRING when omitted."}
								},
								"required":["name","value"]
							}
						},
						"row_limit":{"type":"integer","description":"Maximum rows to return (1..10000). Defaults to 1000. Large result sets are truncated with a marker."}
					},
					"required":["sql"]
				}`),
			},
		})
	}

	// clickhouse_usage_cost: ClickHouse Cloud organization billing report.
	// Gated by the per-agent integration allowlist in helpers.go
	// (restrictedIntegrations) — currently ovad only — AND by the client having
	// authenticated at least once, so the tool never advertises itself when the
	// credentials are unusable.
	if h.canUseIntegration(integrationClickHouse) && h.clickhouseClient != nil && h.clickhouseClient.Ready() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolClickHouseUsageCost,
				Description: "Retrieve the ClickHouse Cloud organization usage-cost report for a date range. Returns the grand total plus a per-day, per-entity breakdown of spend in ClickHouse Credits (CHC), where each entity is a service, data warehouse or ClickPipe, plus a cost-by-category split (compute, storage, backup, data transfer). Use for questions like 'what did we spend on ClickHouse last week / this month' or 'which service is driving ClickHouse cost'. The window is capped at 31 days: to_date may be at most 30 days after from_date. Dates are ISO-8601 (YYYY-MM-DD) and evaluated in UTC. Optionally narrow by resource tag via 'filters' (e.g. \"tag:Environment=Production\").",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"from_date":{"type":"string","description":"Start date of the report window, inclusive. ISO-8601 date YYYY-MM-DD (UTC), e.g. 2024-12-01."},
						"to_date":{"type":"string","description":"End date of the report window, inclusive. ISO-8601 date YYYY-MM-DD (UTC). Must be on or after from_date and at most 30 days after it (max 31-day window)."},
						"filters":{"type":"array","items":{"type":"string"},"description":"Optional resource-tag filters, e.g. [\"tag:Environment=Production\"]. Only tag filters are supported. Omit for the whole organization."}
					},
					"required":["from_date","to_date"]
				}`),
			},
		})
	}

	// clickhouse_query: read-only SQL against the ClickHouse service endpoint.
	// A separate surface from clickhouse_usage_cost — it runs SELECT / SHOW /
	// DESCRIBE / EXISTS against the actual databases and tables. Gated by the
	// per-agent allowlist (ovad only) AND by the query interface having
	// completed its first successful call, so it never advertises itself when
	// the endpoint is unreachable or unconfigured.
	if h.canUseIntegration(integrationClickHouse) && h.clickhouseClient != nil && h.clickhouseClient.QueryReady() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        ToolClickHouseQuery,
				Description: "Run read-only SQL against the configured ClickHouse service and return the result rows as a table. Use for ad-hoc questions over any database and table the connector can read: enumerate databases with SHOW DATABASES, list a database's tables with SHOW TABLES FROM <db> (optionally LIKE '%pattern%'), inspect a table with DESCRIBE <db>.<table>, check existence with EXISTS TABLE <db>.<table>, and count or aggregate rows with SELECT, e.g. SELECT count() FROM <db>.<table> WHERE …. You may send several read-only statements separated by semicolons; ClickHouse returns the result of the LAST one. EVERY statement must be read-only — only SELECT / WITH / SHOW / DESCRIBE / EXISTS / EXPLAIN / VALUES are allowed; INSERT / ALTER / CREATE / DROP / DELETE / SET / SYSTEM and any other write or state change are rejected. When you need to find the right database or table for a request, first discover them with SHOW DATABASES / SHOW TABLES rather than guessing. Quote database and table identifiers that are not simple lowercase words with backticks.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"sql":{"type":"string","description":"Read-only SQL to execute against ClickHouse, e.g. 'SHOW DATABASES', 'SHOW TABLES FROM analytics', 'EXISTS TABLE analytics.findings', or 'SELECT count() FROM analytics.findings'. Only SELECT / WITH / SHOW / DESCRIBE / EXISTS / EXPLAIN / VALUES are permitted; mutations are rejected. Fully-qualify tables as <database>.<table>."},
						"row_limit":{"type":"integer","description":"Maximum rows to return (1..10000). Defaults to 1000. Large result sets are capped server-side and marked truncated."}
					},
					"required":["sql"]
				}`),
			},
		})
	}

	// Freshworks suite (read-only) — gated to the customer-success and
	// product-management agents
	// (restrictedIntegrations: pulse) and per-product on the sub-client being
	// configured, so only the products with credentials advertise their tools.
	if h.canUseIntegration(integrationFreshworks) && h.freshworksClient != nil {
		if h.freshworksClient.Desk.Ready() {
			tools = append(tools,
				llm.Tool{
					Type: "function",
					Function: llm.ToolFunction{
						Name:        ToolFreshdeskListTickets,
						Description: "List recent Freshdesk support tickets, newest-updated first. Optionally filter to tickets updated on or after a timestamp, or to tickets requested by a specific customer email. Returns ticket ID, subject, status, priority and last-updated time. Use for questions like 'show recent support tickets' or 'tickets from bob@customer.com'. To find tickets ASSIGNED to a support agent (e.g. 'tickets on my name'), first call freshdesk_find_agent to get the agent_id, then freshdesk_search_tickets with query 'agent_id:<id>'.",
						Parameters: json.RawMessage(`{
							"type":"object",
							"properties":{
								"updated_since":{"type":"string","description":"Optional RFC3339 timestamp (e.g. 2026-02-01T00:00:00Z). Only tickets updated on or after this instant are returned."},
								"requester_email":{"type":"string","description":"Optional. Only tickets REQUESTED by this contact/customer email are returned. This is the requester, not the assigned agent."},
								"page":{"type":"integer","description":"1-based page number. Omit for the first page."},
								"per_page":{"type":"integer","description":"Results per page, 1..50 (default 20). Keep this small — only the first 20 are rendered, so pulling more just wastes context."}
							}
						}`),
					},
				},
				llm.Tool{
					Type: "function",
					Function: llm.ToolFunction{
						Name:        ToolFreshdeskGetTicket,
						Description: "Get a single Freshdesk ticket by its numeric ID, including its full description and (optionally) its conversation thread of replies and private notes. Use after freshdesk_list_tickets or freshdesk_search_tickets to drill into a ticket.",
						Parameters: json.RawMessage(`{
							"type":"object",
							"properties":{
								"ticket_id":{"type":"integer","description":"Numeric Freshdesk ticket ID."},
								"include_conversations":{"type":"boolean","description":"When true, embed the ticket's replies and notes. Defaults to true."}
							},
							"required":["ticket_id"]
						}`),
					},
				},
				llm.Tool{
					Type: "function",
					Function: llm.ToolFunction{
						Name:        ToolFreshdeskSearchTickets,
						Description: "Search Freshdesk tickets using the Freshdesk filter query syntax, e.g. \"priority:4 AND status:2\" for urgent open tickets, or \"agent_id:123\" or \"tag:'escalated'\". Do NOT wrap the query in quotes yourself. ONLY these fields are valid: status, priority, type, tag, agent_id, group_id, company_id, created_at, updated_at, due_by, fr_due_by, plus CUSTOM fields as cf_<name>. Free-text search and fields like 'company', 'subject', 'description', 'email' or 'name' are NOT supported and return HTTP 400 — do not guess them. To scope tickets to a customer, the reliable way is a customer custom field: call freshdesk_list_ticket_fields to get its exact cf_<name> (e.g. a 'Customer Name' field → cf_customer_name), then query cf_<name>:'<Customer>'. Alternatively use a numeric company_id ('company_id:<id>'), or freshdesk_list_tickets with an exact requester email. Returns matching tickets with status and priority.",
						Parameters: json.RawMessage(`{
							"type":"object",
							"properties":{
								"query":{"type":"string","description":"Freshdesk filter query, e.g. 'priority:4 AND status:2', 'company_id:1234', or a custom-field scope like \"cf_customer_name:'Acme'\". Valid fields ONLY: status, priority, type, tag, agent_id, group_id, company_id, created_at, updated_at, due_by, fr_due_by, cf_<custom>. No free-text; no 'company'/'subject'/'email'/'name' fields."}
							},
							"required":["query"]
						}`),
					},
				},
				llm.Tool{
					Type: "function",
					Function: llm.ToolFunction{
						Name:        ToolFreshdeskFindAgent,
						Description: "Resolve a Freshdesk support agent by email or name and return their numeric agent_id. Use this first when asked for tickets ASSIGNED to a person (e.g. 'tickets on my name' / 'tickets assigned to Jane'), then pass the agent_id into freshdesk_search_tickets as query 'agent_id:<id>'. Email is the reliable key; name does a case-insensitive substring match.",
						Parameters: json.RawMessage(`{
							"type":"object",
							"properties":{
								"email":{"type":"string","description":"Agent email address (preferred, exact match)."},
								"name":{"type":"string","description":"Agent full or partial name (case-insensitive substring match). Used only when email is not provided."}
							}
						}`),
					},
				},
				llm.Tool{
					Type: "function",
					Function: llm.ToolFunction{
						Name:        ToolFreshdeskListTicketFields,
						Description: "List Freshdesk ticket fields (system + custom). Use this to discover the exact cf_<name> key for a custom attribute (e.g. a 'Customer Name' field) so you can scope a ticket search with freshdesk_search_tickets query cf_<name>:'<value>'. Takes no arguments.",
						Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
					},
				},
			)
		}
		if h.freshworksClient.Chat.Ready() {
			tools = append(tools,
				llm.Tool{
					Type: "function",
					Function: llm.ToolFunction{
						Name:        ToolFreshchatGetConversation,
						Description: "Get a Freshchat conversation header by its conversation ID (status, channel, assigned agent, and any inline messages). Use freshchat_get_conversation_messages to read the full message thread.",
						Parameters: json.RawMessage(`{
							"type":"object",
							"properties":{
								"conversation_id":{"type":"string","description":"Freshchat conversation ID."}
							},
							"required":["conversation_id"]
						}`),
					},
				},
				llm.Tool{
					Type: "function",
					Function: llm.ToolFunction{
						Name:        ToolFreshchatGetConversationMessages,
						Description: "Get the messages in a Freshchat conversation by conversation ID. Returns each message's actor (user/agent/system), timestamp and text. Use to read what was said in a live-chat conversation.",
						Parameters: json.RawMessage(`{
							"type":"object",
							"properties":{
								"conversation_id":{"type":"string","description":"Freshchat conversation ID."},
								"page":{"type":"integer","description":"1-based page of messages. Omit for the first page."}
							},
							"required":["conversation_id"]
						}`),
					},
				},
			)
		}
		if h.freshworksClient.CRM.Ready() {
			tools = append(tools,
				llm.Tool{
					Type: "function",
					Function: llm.ToolFunction{
						Name:        ToolFreshworksCRMSearch,
						Description: "Search the Freshworks CRM (Freshsales) for contacts, deals and accounts matching a term (name, email, company, etc.). Returns each hit's name, type and ID. Use the returned IDs with freshworks_crm_get_contact or freshworks_crm_get_deal to fetch details.",
						Parameters: json.RawMessage(`{
							"type":"object",
							"properties":{
								"query":{"type":"string","description":"Search term, e.g. a person's name, email, or company."},
								"entities":{"type":"array","items":{"type":"string"},"description":"Optional record types to search: contact, deal, sales_account, user, lead. Defaults to contact, deal and sales_account."}
							},
							"required":["query"]
						}`),
					},
				},
				llm.Tool{
					Type: "function",
					Function: llm.ToolFunction{
						Name:        ToolFreshworksCRMGetContact,
						Description: "Get a Freshworks CRM contact by its numeric ID (name, title, email, phone, location). Use after freshworks_crm_search.",
						Parameters: json.RawMessage(`{
							"type":"object",
							"properties":{
								"contact_id":{"type":"integer","description":"Numeric CRM contact ID."}
							},
							"required":["contact_id"]
						}`),
					},
				},
				llm.Tool{
					Type: "function",
					Function: llm.ToolFunction{
						Name:        ToolFreshworksCRMGetDeal,
						Description: "Get a Freshworks CRM deal/opportunity by its numeric ID (name, amount, stage, probability, expected/closed dates). Use after freshworks_crm_search.",
						Parameters: json.RawMessage(`{
							"type":"object",
							"properties":{
								"deal_id":{"type":"integer","description":"Numeric CRM deal ID."}
							},
							"required":["deal_id"]
						}`),
					},
				},
			)
		}
	}

	// http_get: read-only outbound HTTP for workflows that need upstream
	// metadata (release JSON, raw Chart.yaml on GitHub, version files, etc.).
	// See commands/helpers.go for SSRF guards.
	tools = append(tools, llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        ToolHTTPGet,
			Description: "Perform a read-only HTTP GET against a public URL and return the response body (truncated). Intended for fetching public metadata such as GitHub releases JSON (e.g. https://api.github.com/repos/<owner>/<repo>/releases/latest), raw files on GitHub (e.g. https://raw.githubusercontent.com/<owner>/<repo>/<branch>/<path>), or other small public JSON / YAML / text endpoints. Only http(s) schemes are allowed; the request must NOT carry credentials. For Helm chart version checks, fetch the chart's upstream `Chart.yaml` directly from its source repo on GitHub (raw.githubusercontent.com) instead of trying to scrape a Helm index or an OCI registry. Do NOT use to circumvent existing tools (use get_file_content, github_*, datadog_*, etc. when applicable).",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"url":{"type":"string","description":"Full https:// (or http://) URL to GET. No auth headers, no body, no redirects to private hosts."},
					"accept":{"type":"string","description":"Optional Accept header. Defaults to 'application/json, application/yaml, text/yaml, text/plain, */*'."},
					"max_bytes":{"type":"integer","description":"Cap on the response body returned to the model (1..1048576). Defaults to 200000. Larger responses are truncated with a marker."}
				},
				"required":["url"]
			}`),
		},
	})

	tools = append(tools, h.dashboardTools()...)
	tools = append(tools, h.workflowTools()...)

	return tools
}

// resolveJiraReporterByEmail resolves an email address to a Jira accountId for
// the Reporter field via the Jira user search (email is the exact, reliable
// key). It returns empty strings when no Jira account matches. The email is
// never logged (CWE-312).
func (h *GeneralHandler) resolveJiraReporterByEmail(email string) (accountID, displayName string) {
	email = strings.TrimSpace(email)
	if email == "" || h.jiraClient == nil {
		return "", ""
	}
	users, err := h.jiraClient.SearchUsersGeneral(email)
	if err != nil {
		log.Printf("reporter resolution: Jira user search by email failed: %v", err)
		return "", ""
	}
	if len(users) > 0 {
		return users[0].AccountID, users[0].DisplayName
	}
	return "", ""
}

// resolveRequesterReporter maps the originating human Slack requester to their
// Jira accountId so a created ticket can record Reporter = the real person who
// asked for it (auditability/compliance). It reuses the Slack email resolver
// chain: Slack user_id → email (users.info) → Jira accountId (email-first, then
// a confident display-name match).
//
// It returns empty strings when the requester cannot be confidently resolved —
// no Slack user id (e.g. a scheduled workflow tick), a profile with no email
// and no unique name match, or no matching Jira account — in which case callers
// leave the reporter unset so Jira defaults it to the bot/service account. The
// requester's email is never logged (CWE-312); only non-sensitive identifiers
// appear in logs.
func (h *GeneralHandler) resolveRequesterReporter(userID string) (accountID, displayName string) {
	userID = strings.TrimSpace(userID)
	if userID == "" || h.slackClient == nil || h.jiraClient == nil {
		return "", ""
	}
	user, err := h.slackClient.GetUserInfo(userID)
	if err != nil {
		log.Printf("[user=%s] reporter resolution: Slack users.info lookup failed: %v", userID, err)
		return "", ""
	}
	// Email is the most reliable key.
	if id, name := h.resolveJiraReporterByEmail(user.Profile.Email); id != "" {
		return id, name
	}
	// Fallback: resolve by display name, but only accept a confident match so a
	// compliance-sensitive field is never attributed to the wrong person.
	realName := strings.TrimSpace(user.RealName)
	if realName != "" {
		if users, err := h.jiraClient.SearchUsersGeneral(realName); err != nil {
			log.Printf("[user=%s] reporter resolution: Jira user search by name %q failed: %v", userID, realName, err)
		} else if best, ok := atlassian.BestUserMatch(users, realName); ok {
			return best.AccountID, best.DisplayName
		}
	}
	return "", ""
}

func (h *GeneralHandler) executeTool(ctx context.Context, channelID, userID, auditTS, name, argsJSON string) string {
	if out, handled := h.executeDashboardTool(ctx, userID, channelID, name, argsJSON); handled {
		return out
	}
	if out, handled := h.executeWorkflowTool(ctx, userID, channelID, name, argsJSON); handled {
		return out
	}
	// Defense in depth: integration-specific tools are only advertised to the
	// agents allowed by restrictedIntegrations, but reject the call outright if
	// another agent's model fabricates one it was never offered.
	if h.toolDenied(name) {
		return preconditionErrf("Error: %s is not available to the %s agent.", name, h.agentID)
	}
	switch name {
	case ToolListOrgRepos:
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		repos, err := h.ghClient.ListOrgRepos(ctx, owner)
		if err != nil {
			return fmt.Sprintf("Error listing org repos: %v", err)
		}
		if len(repos) == 0 {
			return fmt.Sprintf("No repositories found for organization %s.", owner)
		}
		log.Printf("[user=%s channel=%s] listed %d org repos for %s", userID, channelID, len(repos), owner)
		return fmt.Sprintf("Organization: %s\nRepositories (%d):\n%s", owner, len(repos), strings.Join(repos, "\n"))

	case ToolListUserRepos:
		repos, err := h.ghClient.ListUserRepos(ctx)
		if err != nil {
			return fmt.Sprintf("Error listing user repos: %v", err)
		}
		if len(repos) == 0 {
			return "No repositories found for the authenticated user."
		}
		log.Printf("[user=%s channel=%s] listed %d user repos", userID, channelID, len(repos))
		return fmt.Sprintf("Repositories (%d):\n%s", len(repos), strings.Join(repos, "\n"))

	case ToolListRepoTeams:
		args, errMsg := parseToolArgs[struct {
			Repo string `json:"repo"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		teams, err := h.ghClient.ListRepoTeams(ctx, owner, args.Repo)
		if err != nil {
			return fmt.Sprintf("Error listing teams for %s/%s: %v", owner, args.Repo, err)
		}
		if len(teams) == 0 {
			return fmt.Sprintf("No teams have direct access to %s/%s.", owner, args.Repo)
		}
		lines := make([]string, 0, len(teams))
		for _, t := range teams {
			lines = append(lines, fmt.Sprintf("- @%s/%s (%s) — permission: %s", owner, t.Slug, t.Name, t.Permission))
		}
		log.Printf("[user=%s channel=%s] listed %d teams for %s/%s", userID, channelID, len(teams), owner, args.Repo)
		return fmt.Sprintf("Teams with direct access to %s/%s (%d):\n%s", owner, args.Repo, len(teams), strings.Join(lines, "\n"))

	case ToolListRepoTeamsBulk:
		args, errMsg := parseToolArgs[struct {
			Repos []string `json:"repos"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		// Dedupe + drop empties so the LLM can be sloppy.
		seen := make(map[string]bool, len(args.Repos))
		repos := make([]string, 0, len(args.Repos))
		for _, r := range args.Repos {
			r = strings.TrimSpace(r)
			if r == "" || seen[r] {
				continue
			}
			seen[r] = true
			repos = append(repos, r)
		}
		if len(repos) == 0 {
			return "Error: list_repo_teams_bulk requires a non-empty `repos` array."
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		results, err := h.ghClient.ListReposTeamsBulk(ctx, owner, repos)
		if err != nil {
			return fmt.Sprintf("Error listing teams in bulk: %v", err)
		}
		var sb strings.Builder
		okCount, errCount := 0, 0
		for _, r := range results {
			if r.Error != "" {
				errCount++
				fmt.Fprintf(&sb, "\n=== %s/%s — ERROR: %s ===\n", owner, r.Repo, r.Error)
				continue
			}
			okCount++
			if len(r.Teams) == 0 {
				fmt.Fprintf(&sb, "\n=== %s/%s (no teams with direct access) ===\n", owner, r.Repo)
				continue
			}
			fmt.Fprintf(&sb, "\n=== %s/%s (%d teams) ===\n", owner, r.Repo, len(r.Teams))
			for _, t := range r.Teams {
				fmt.Fprintf(&sb, "- @%s/%s (%s) — permission: %s\n", owner, t.Slug, t.Name, t.Permission)
			}
		}
		log.Printf("[user=%s channel=%s] list_repo_teams_bulk: %d ok, %d errors (of %d repos)", userID, channelID, okCount, errCount, len(repos))
		return fmt.Sprintf("Bulk list_repo_teams: %d ok, %d errors (of %d repos):%s", okCount, errCount, len(repos), sb.String())

	case ToolGetFilesBulk:
		args, errMsg := parseToolArgs[struct {
			Files []github.FileFetchSpec `json:"files"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		// Drop entries missing repo or path so a single bad entry doesn't
		// abort the whole call (kept consistent with get_file_content's
		// empty-arg guard).
		clean := make([]github.FileFetchSpec, 0, len(args.Files))
		for _, f := range args.Files {
			if strings.TrimSpace(f.Repo) == "" || strings.TrimSpace(f.Path) == "" {
				continue
			}
			clean = append(clean, f)
		}
		if len(clean) == 0 {
			return "Error: get_files_bulk requires a non-empty `files` array where every entry has both `repo` and `path`."
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		results, err := h.ghClient.GetFilesBulk(ctx, owner, clean)
		if err != nil {
			return fmt.Sprintf("Error fetching files in bulk: %v", err)
		}
		var sb strings.Builder
		okCount, errCount := 0, 0
		for _, r := range results {
			if r.Error != "" {
				errCount++
				fmt.Fprintf(&sb, "\n=== %s:%s — ERROR: %s ===\n", r.Repo, r.Path, r.Error)
				continue
			}
			okCount++
			content := r.Content
			if len(content) > maxFileContentLen {
				totalLen := len(content)
				content = content[:maxFileContentLen] + fmt.Sprintf(
					"\n... (TRUNCATED — %d of %d chars shown)",
					maxFileContentLen, totalLen)
			}
			fmt.Fprintf(&sb, "\n=== %s:%s ===\n%s\n", r.Repo, r.Path, content)
		}
		log.Printf("[user=%s channel=%s] get_files_bulk: %d ok, %d errors (of %d files)", userID, channelID, okCount, errCount, len(clean))
		return fmt.Sprintf("Bulk get_file_content: %d ok, %d errors (of %d files):%s", okCount, errCount, len(clean), sb.String())

	case ToolGetFileContent:
		args, errMsg := parseToolArgs[struct {
			Repo   string `json:"repo"`
			Path   string `json:"path"`
			Branch string `json:"branch"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, branch, errMsg := resolveRepoBranch(ctx, h.ghClient, args.Repo, args.Branch)
		if errMsg != "" {
			return errMsg
		}
		content, _, err := h.ghClient.GetFileContent(ctx, owner, args.Repo, args.Path, branch)
		if err != nil {
			hint := ""
			if strings.Contains(err.Error(), "404") {
				hint = " This path may be a directory, or it may be nested under a provider subdirectory (e.g. aws/, azure/). Try list_directory on the parent path to discover the correct structure, then read the files you need."
			}
			return fmt.Sprintf("Error reading file: %v.%s", err, hint)
		}
		if len(content) > maxFileContentLen {
			totalLen := len(content)
			content = content[:maxFileContentLen] + fmt.Sprintf(
				"\n... (TRUNCATED — showing %d of %d chars, %.0f%% of file hidden. "+
					"If you need to modify content beyond this point, use modify_file with a smaller, "+
					"known anchor from the visible portion, or ask the user for the exact text to change.)",
				maxFileContentLen, totalLen, float64(totalLen-maxFileContentLen)/float64(totalLen)*100)
		}
		return content

	case ToolGetRepoDefaultBranch:
		args, errMsg := parseToolArgs[struct {
			Repo string `json:"repo"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		branch, err := h.ghClient.GetDefaultBranch(ctx, owner, args.Repo)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return fmt.Sprintf("Default branch for %s: %s", args.Repo, branch)

	case ToolGetAuthenticatedUser:
		user, err := h.ghClient.GetAuthenticatedUser(ctx)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return fmt.Sprintf("Authenticated as: %s", user)

	case ToolResolveOwner:
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return fmt.Sprintf("Resolved owner: %s", owner)

	case ToolSearchFiles:
		args, errMsg := parseToolArgs[struct {
			Repo    string `json:"repo"`
			Pattern string `json:"pattern"`
			Branch  string `json:"branch"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, branch, errMsg := resolveRepoBranch(ctx, h.ghClient, args.Repo, args.Branch)
		if errMsg != "" {
			return errMsg
		}
		matches, err := h.ghClient.SearchFiles(ctx, owner, args.Repo, branch, args.Pattern)
		if err != nil {
			return fmt.Sprintf("Error searching files: %v", err)
		}
		if len(matches) == 0 {
			return fmt.Sprintf("No files matching '%s' found in %s.", args.Pattern, args.Repo)
		}
		log.Printf("[user=%s channel=%s] searched files in %s for '%s' (%d matches)", userID, channelID, args.Repo, args.Pattern, len(matches))
		if len(matches) > 50 {
			matches = matches[:50]
			return fmt.Sprintf("Found %d+ matches (showing first 50):\n%s", len(matches), strings.Join(matches, "\n"))
		}
		return fmt.Sprintf("Found %d matches:\n%s", len(matches), strings.Join(matches, "\n"))

	case ToolListDirectory:
		args, errMsg := parseToolArgs[struct {
			Repo   string `json:"repo"`
			Path   string `json:"path"`
			Branch string `json:"branch"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, branch, errMsg := resolveRepoBranch(ctx, h.ghClient, args.Repo, args.Branch)
		if errMsg != "" {
			return errMsg
		}
		entries, err := h.ghClient.GetDirectoryContents(ctx, owner, args.Repo, args.Path, branch)
		if err != nil {
			return fmt.Sprintf("Error listing directory: %v", err)
		}
		log.Printf("[user=%s channel=%s] listed directory %s/%s/%s (%d entries)", userID, channelID, args.Repo, branch, args.Path, len(entries))
		return fmt.Sprintf("Contents of %s/%s:\n%s", args.Repo, args.Path, strings.Join(entries, "\n"))

	case ToolFetchChannelContext:
		context, err := h.contextProvider.GetChannelContext(channelID)
		if err != nil {
			return fmt.Sprintf("Error fetching channel context: %v", err)
		}
		log.Printf("[user=%s channel=%s] fetched channel context via tool", userID, channelID)
		return context

	case ToolModifyFile:
		args, errMsg := parseToolArgs[struct {
			Repo        string `json:"repo"`
			Path        string `json:"path"`
			OldContent  string `json:"old_content"`
			NewContent  string `json:"new_content"`
			Description string `json:"description"`
			PRBody      string `json:"pr_body"`
			PRTitle     string `json:"pr_title"`
			BranchName  string `json:"branch_name"`
			Branch      string `json:"branch"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, baseBranch, errMsg := resolveRepoBranch(ctx, h.ghClient, args.Repo, args.Branch)
		if errMsg != "" {
			return errMsg
		}

		readBranch := h.branchMgr.ReadBranch(owner, args.Repo, baseBranch)
		fullContent, fileSHA, err := h.ghClient.GetFileContent(ctx, owner, args.Repo, args.Path, readBranch)
		if err != nil {
			return preconditionErrf("Error reading current file: %v", err)
		}
		if !strings.Contains(fullContent, args.OldContent) {
			return preconditionErrf("Error: old_content not found in the file. Make sure old_content is an exact substring of the current file (including whitespace and indentation). Re-read the file with get_file_content and try again.")
		}
		occurrences := strings.Count(fullContent, args.OldContent)
		if occurrences > 1 {
			return preconditionErrf("Error: old_content matches %d locations in the file. Include more surrounding context lines to make it unique.", occurrences)
		}
		updatedContent := strings.Replace(fullContent, args.OldContent, args.NewContent, 1)
		if updatedContent == fullContent {
			return preconditionErrf("Error: old_content and new_content produce identical file content. The replacement is a no-op — double-check that new_content differs from old_content.")
		}
		// Reject changes whose only effect is whitespace (trailing newline, spaces,
		// blank lines). A scheduled auto-fix workflow that submits a whitespace-only
		// PR is almost always the model giving up after its real replacement
		// failed, and it pollutes the repo with useless PRs.
		if stripWhitespace(updatedContent) == stripWhitespace(fullContent) {
			return preconditionErrf("Error: the only effective change is whitespace (trailing newline / spaces / blank lines). " +
				"This is rejected because it is not a real code change. " +
				"Re-read the file with get_file_content, then submit a modify_file with a substantive content difference.")
		}

		prBody := buildPRBody(userID, args.PRBody, fmt.Sprintf("Automated change requested via Slack by <@%s>.\n\nChange: %s", userID, args.Description))
		result, err := h.branchMgr.CommitAndPR(ctx, owner, args.Repo, baseBranch, userID, args.Description, prBody, args.BranchName, args.PRTitle, []string{args.Path},
			func(branch string) error {
				commitMsg := fmt.Sprintf("%s: %s", h.agentID, args.Description)
				return h.ghClient.UpdateFile(ctx, owner, args.Repo, args.Path, branch, commitMsg, []byte(updatedContent), fileSHA)
			})
		if err != nil {
			return commitErrResult(err)
		}
		log.Printf("[user=%s channel=%s] modify_file: PR %s (new=%t)", userID, channelID, result.PrURL, result.IsNew)
		if result.IsNew {
			return fmt.Sprintf("Pull request created: %s", result.PrURL)
		}
		return fmt.Sprintf("Changes committed to existing PR: %s", result.PrURL)

	case ToolCreateFile:
		args, errMsg := parseToolArgs[struct {
			Repo        string `json:"repo"`
			Path        string `json:"path"`
			Content     string `json:"content"`
			Description string `json:"description"`
			PRBody      string `json:"pr_body"`
			PRTitle     string `json:"pr_title"`
			BranchName  string `json:"branch_name"`
			Branch      string `json:"branch"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, baseBranch, errMsg := resolveRepoBranch(ctx, h.ghClient, args.Repo, args.Branch)
		if errMsg != "" {
			return errMsg
		}

		prBody := buildPRBody(userID, args.PRBody, fmt.Sprintf("Automated file creation requested via Slack by <@%s>.\n\nChange: %s\nNew file: `%s`", userID, args.Description, args.Path))
		result, err := h.branchMgr.CommitAndPR(ctx, owner, args.Repo, baseBranch, userID, args.Description, prBody, args.BranchName, args.PRTitle, []string{args.Path},
			func(branch string) error {
				commitMsg := fmt.Sprintf("%s: %s", h.agentID, args.Description)
				return h.ghClient.CreateFile(ctx, owner, args.Repo, args.Path, branch, commitMsg, []byte(args.Content))
			})
		if err != nil {
			return commitErrResult(err)
		}
		log.Printf("[user=%s channel=%s] create_file: PR %s (new=%t)", userID, channelID, result.PrURL, result.IsNew)
		if result.IsNew {
			return fmt.Sprintf("File created and pull request opened: %s", result.PrURL)
		}
		return fmt.Sprintf("File created and committed to existing PR: %s", result.PrURL)

	case ToolRegexReplaceFile:
		args, errMsg := parseToolArgs[struct {
			Repo        string `json:"repo"`
			Path        string `json:"path"`
			Pattern     string `json:"pattern"`
			Replacement string `json:"replacement"`
			Description string `json:"description"`
			PRBody      string `json:"pr_body"`
			PRTitle     string `json:"pr_title"`
			BranchName  string `json:"branch_name"`
			Branch      string `json:"branch"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		re, err := regexp.Compile(args.Pattern)
		if err != nil {
			return preconditionErrf("Error compiling regex pattern: %v", err)
		}
		owner, baseBranch, errMsg := resolveRepoBranch(ctx, h.ghClient, args.Repo, args.Branch)
		if errMsg != "" {
			return errMsg
		}

		readBranch := h.branchMgr.ReadBranch(owner, args.Repo, baseBranch)
		fullContent, fileSHA, err := h.ghClient.GetFileContent(ctx, owner, args.Repo, args.Path, readBranch)
		if err != nil {
			return preconditionErrf("Error reading current file: %v", err)
		}

		updatedContent := re.ReplaceAllString(fullContent, args.Replacement)
		if updatedContent == fullContent {
			return preconditionErrf("Error: regex pattern matched nothing in the file. Double-check the pattern and try again.")
		}
		matches := len(re.FindAllStringIndex(fullContent, -1))
		log.Printf("[user=%s channel=%s] regex_replace_file: %d matches of %q in %s/%s",
			userID, channelID, matches, args.Pattern, args.Repo, args.Path)

		prBody := buildPRBody(userID, args.PRBody, fmt.Sprintf("Automated regex replacement requested via Slack by <@%s>.\n\nChange: %s\nPattern: `%s` → `%s`\nMatches replaced: %d", userID, args.Description, args.Pattern, args.Replacement, matches))
		result, err := h.branchMgr.CommitAndPR(ctx, owner, args.Repo, baseBranch, userID, args.Description, prBody, args.BranchName, args.PRTitle, []string{args.Path},
			func(branch string) error {
				commitMsg := fmt.Sprintf("%s: %s", h.agentID, args.Description)
				return h.ghClient.UpdateFile(ctx, owner, args.Repo, args.Path, branch, commitMsg, []byte(updatedContent), fileSHA)
			})
		if err != nil {
			return commitErrResult(err)
		}
		log.Printf("[user=%s channel=%s] regex_replace_file: PR %s (new=%t, matches=%d)", userID, channelID, result.PrURL, result.IsNew, matches)
		if result.IsNew {
			return fmt.Sprintf("Replaced %d matches. Pull request created: %s", matches, result.PrURL)
		}
		return fmt.Sprintf("Replaced %d matches. Changes committed to existing PR: %s", matches, result.PrURL)

	case ToolGetPullRequest:
		args, errMsg := parseToolArgs[struct {
			Repo   string `json:"repo"`
			Number int    `json:"number"`
			URL    string `json:"url"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		// If a URL was provided, extract owner/repo/number from it.
		if args.URL != "" {
			prOwner, prRepo, prNum, parseErr := github.ParsePRURL(args.URL)
			if parseErr != nil {
				return fmt.Sprintf("Error parsing PR URL: %v", parseErr)
			}
			owner = prOwner
			args.Repo = prRepo
			args.Number = prNum
		}
		if args.Number == 0 {
			return "Error: PR number or URL is required."
		}
		pr, err := h.ghClient.GetPullRequest(ctx, owner, args.Repo, args.Number)
		if err != nil {
			return fmt.Sprintf("Error getting PR: %v", err)
		}
		log.Printf("[user=%s channel=%s] fetched PR #%d in %s/%s", userID, channelID, args.Number, owner, args.Repo)
		return github.FormatPRSummary(pr)

	case ToolListPullRequests:
		args, errMsg := parseToolArgs[struct {
			Repo  string `json:"repo"`
			State string `json:"state"`
			Limit int    `json:"limit"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		prs, err := h.ghClient.ListPullRequests(ctx, owner, args.Repo, args.State, args.Limit)
		if err != nil {
			return fmt.Sprintf("Error listing PRs: %v", err)
		}
		if len(prs) == 0 {
			return fmt.Sprintf("No pull requests found in %s (state: %s).", args.Repo, args.State)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Pull Requests in %s (%d):\n", args.Repo, len(prs))
		for _, pr := range prs {
			fmt.Fprintf(&sb, "  • #%d %s (%s) by %s — %s\n", pr.Number, pr.Title, pr.State, pr.Author, pr.URL)
		}
		log.Printf("[user=%s channel=%s] listed %d PRs in %s", userID, channelID, len(prs), args.Repo)
		return sb.String()

	case ToolListCommits:
		args, errMsg := parseToolArgs[struct {
			Repo   string `json:"repo"`
			Branch string `json:"branch"`
			Path   string `json:"path"`
			Author string `json:"author"`
			Since  string `json:"since"`
			Until  string `json:"until"`
			Limit  int    `json:"limit"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.Repo == "" {
			return "Error: repo is required."
		}
		var since, until time.Time
		if args.Since != "" {
			t, err := time.Parse(time.RFC3339, args.Since)
			if err != nil {
				return fmt.Sprintf("Error: invalid since timestamp %q (want RFC3339 like 2026-04-21T00:00:00Z): %v", args.Since, err)
			}
			since = t
		}
		if args.Until != "" {
			t, err := time.Parse(time.RFC3339, args.Until)
			if err != nil {
				return fmt.Sprintf("Error: invalid until timestamp %q (want RFC3339 like 2026-04-21T23:59:59Z): %v", args.Until, err)
			}
			until = t
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		commits, err := h.ghClient.ListCommits(ctx, owner, args.Repo, args.Branch, args.Author, args.Path, since, until, args.Limit)
		if err != nil {
			return fmt.Sprintf("Error listing commits: %v", err)
		}
		if len(commits) == 0 {
			return fmt.Sprintf("No commits found in %s (branch=%q, path=%q, author=%q, since=%q, until=%q).", args.Repo, args.Branch, args.Path, args.Author, args.Since, args.Until)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Commits in %s (%d):\n", args.Repo, len(commits))
		for _, cm := range commits {
			fmt.Fprintf(&sb, "  • %s %s — @%s (%s) %s\n", cm.SHA, cm.Message, cm.Author, cm.Date.UTC().Format(time.RFC3339), cm.URL)
		}
		log.Printf("[user=%s channel=%s] listed %d commits in %s", userID, channelID, len(commits), args.Repo)
		return sb.String()

	case ToolSearchCode:
		args, errMsg := parseToolArgs[struct {
			Repo  string `json:"repo"`
			Query string `json:"query"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		results, err := h.ghClient.SearchCode(ctx, owner, args.Repo, args.Query)
		if err != nil {
			return fmt.Sprintf("Error searching code: %v", err)
		}
		if len(results) == 0 {
			return fmt.Sprintf("No code matches found for '%s' in %s. Try different search terms, broader patterns, or check if the repository name is correct.", args.Query, args.Repo)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Code search results for '%s' in %s (%d matches):\n", args.Query, args.Repo, len(results))
		for _, r := range results {
			fmt.Fprintf(&sb, "\n• %s\n  %s\n", r.File, r.URL)
			for _, frag := range r.Fragments {
				fmt.Fprintf(&sb, "  ```\n  %s\n  ```\n", frag)
			}
		}
		log.Printf("[user=%s channel=%s] searched code in %s for '%s' (%d matches)", userID, channelID, args.Repo, args.Query, len(results))
		return sb.String()

	case ToolSearchCodeOrg:
		args, errMsg := parseToolArgs[struct {
			Query string `json:"query"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		results, err := h.ghClient.SearchCodeOrg(ctx, owner, args.Query)
		if err != nil {
			return fmt.Sprintf("Error searching code across org: %v", err)
		}
		if len(results) == 0 {
			return fmt.Sprintf("No code matches found for '%s' across the org. Try different search terms or broader patterns.", args.Query)
		}
		// Group results by repo for a clear summary.
		repoResults := make(map[string][]github.CodeSearchResult)
		var repoOrder []string
		for _, r := range results {
			if _, seen := repoResults[r.Repo]; !seen {
				repoOrder = append(repoOrder, r.Repo)
			}
			repoResults[r.Repo] = append(repoResults[r.Repo], r)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Code search results for '%s' across org '%s' (%d matches in %d repos):\n", args.Query, owner, len(results), len(repoOrder))
		for _, repo := range repoOrder {
			hits := repoResults[repo]
			fmt.Fprintf(&sb, "\n=== %s (%d matches) ===\n", repo, len(hits))
			for _, r := range hits {
				fmt.Fprintf(&sb, "  • %s\n    %s\n", r.File, r.URL)
				for _, frag := range r.Fragments {
					fmt.Fprintf(&sb, "    ```\n    %s\n    ```\n", frag)
				}
			}
		}
		log.Printf("[user=%s channel=%s] searched code across org '%s' for '%s' (%d matches in %d repos)", userID, channelID, owner, args.Query, len(results), len(repoOrder))
		return sb.String()

	case ToolGetWorkflowRun:
		args, errMsg := parseToolArgs[struct {
			URL string `json:"url"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, repo, runID, err := github.ParseWorkflowRunURL(args.URL)
		if err != nil {
			return fmt.Sprintf("Error parsing workflow run URL: %v", err)
		}
		log.Printf("[user=%s channel=%s] fetching workflow run %s/%s/%d", userID, channelID, owner, repo, runID)
		summary, err := h.ghClient.GetWorkflowRunSummary(ctx, owner, repo, runID)
		if err != nil {
			return fmt.Sprintf("Error fetching workflow run: %v", err)
		}
		result := github.FormatWorkflowRunSummary(summary)
		log.Printf("[user=%s channel=%s] fetched workflow run %s/%s/%d (conclusion: %s)", userID, channelID, owner, repo, runID, summary.Conclusion)
		return result

	case ToolRerunFailedJobs:
		args, errMsg := parseToolArgs[struct {
			URL string `json:"url"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, repo, runID, err := github.ParseWorkflowRunURL(args.URL)
		if err != nil {
			return fmt.Sprintf("Error parsing workflow run URL: %v", err)
		}
		log.Printf("[user=%s channel=%s] rerunning failed jobs for %s/%s/%d", userID, channelID, owner, repo, runID)
		if err := h.ghClient.RerunFailedJobs(ctx, owner, repo, runID); err != nil {
			return fmt.Sprintf("Error rerunning failed jobs: %v", err)
		}
		log.Printf("[user=%s channel=%s] successfully triggered rerun of failed jobs for %s/%s/%d", userID, channelID, owner, repo, runID)
		return fmt.Sprintf("Successfully triggered re-run of failed jobs for workflow run %d in %s/%s. The run is now in progress: %s", runID, owner, repo, args.URL)

	case ToolRerunWorkflow:
		args, errMsg := parseToolArgs[struct {
			URL string `json:"url"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		owner, repo, runID, err := github.ParseWorkflowRunURL(args.URL)
		if err != nil {
			return fmt.Sprintf("Error parsing workflow run URL: %v", err)
		}
		log.Printf("[user=%s channel=%s] rerunning entire workflow %s/%s/%d", userID, channelID, owner, repo, runID)
		if err := h.ghClient.RerunWorkflow(ctx, owner, repo, runID); err != nil {
			return fmt.Sprintf("Error rerunning workflow: %v", err)
		}
		log.Printf("[user=%s channel=%s] successfully triggered full rerun of %s/%s/%d", userID, channelID, owner, repo, runID)
		return fmt.Sprintf("Successfully triggered full re-run of workflow run %d in %s/%s. All jobs will run again: %s", runID, owner, repo, args.URL)

	case ToolReplyInThread:
		args, errMsg := parseToolArgs[struct {
			ThreadTS string `json:"thread_ts"`
			Text     string `json:"text"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if err := h.slackClient.PostThreadReply(channelID, args.ThreadTS, args.Text); err != nil {
			return fmt.Sprintf("Error posting thread reply: %v", err)
		}
		log.Printf("[user=%s channel=%s] posted thread reply to ts=%s", userID, channelID, args.ThreadTS)
		return "Successfully posted reply in thread."

	case ToolPostSlackMessage:
		args, errMsg := parseToolArgs[struct {
			ChannelID string `json:"channel_id"`
			Text      string `json:"text"`
			ThreadTS  string `json:"thread_ts"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.ChannelID) == "" || strings.TrimSpace(args.Text) == "" {
			return preconditionErrf("Error: 'channel_id' and 'text' are required.")
		}
		// Headless safety net: a scheduled tick has no interactive/requesting
		// user. When the model cannot resolve the real person behind a piece of
		// output it sometimes falls back to the workflow creator's identity
		// (injected only for operator logging) and emits an active <@creator>
		// ping — e.g. mis-attributing a Jira label to the creator. Strip the
		// creator's own self-mention so a scheduled summary can never ping the
		// creator merely because attribution failed. Correct attribution
		// (get_jira_label_author) is the real fix; this is the backstop.
		if h.headless {
			if cleaned, changed := neutralizeUserPing(args.Text, userID); changed {
				log.Printf("[user=%s channel=%s] post_slack_message: stripped creator self-mention <@%s> from headless output (likely failed-attribution fallback)", userID, channelID, userID)
				args.Text = cleaned
			}
		}
		// In a headless workflow tick, guard against the LLM posting the exact
		// same message twice in one run — a known tool-loop slip that produces
		// duplicate top-level reports in the channel. Identical (channel,
		// thread_ts, text) posts are deduplicated: the first does the real
		// Slack post; any repeat returns the original message's ts WITHOUT
		// posting again, so the model still gets a valid thread parent for its
		// follow-up reply.
		sig := args.ChannelID + "\x00" + strings.TrimSpace(args.ThreadTS) + "\x00" + args.Text
		if h.headless {
			if prevTS, ok := h.postedSlackSigs[sig]; ok {
				log.Printf("[user=%s channel=%s] post_slack_message dedup: identical message already posted to %s (ts=%s); skipping duplicate", userID, channelID, args.ChannelID, prevTS)
				if strings.TrimSpace(args.ThreadTS) != "" {
					return fmt.Sprintf("Already posted this exact threaded reply to channel %s under thread_ts=%s (ts=%s). Did not post a duplicate.", args.ChannelID, args.ThreadTS, prevTS)
				}
				return fmt.Sprintf("Already posted this exact message to channel %s (ts=%s). Did not post a duplicate — use ts=%s as the thread parent for any follow-up reply.", args.ChannelID, prevTS, prevTS)
			}
		}
		ts, err := h.slackClient.PostMessageInThread(args.ChannelID, args.ThreadTS, args.Text)
		if err != nil {
			return fmt.Sprintf("Error posting to channel %s: %v", args.ChannelID, err)
		}
		if h.headless {
			if h.postedSlackSigs == nil {
				h.postedSlackSigs = make(map[string]string)
			}
			h.postedSlackSigs[sig] = ts
		}
		if strings.TrimSpace(args.ThreadTS) != "" {
			log.Printf("[user=%s channel=%s] posted threaded reply to %s (thread_ts=%s, ts=%s)", userID, channelID, args.ChannelID, args.ThreadTS, ts)
			return fmt.Sprintf("Successfully posted threaded reply to channel %s under thread_ts=%s (ts=%s).", args.ChannelID, args.ThreadTS, ts)
		}
		log.Printf("[user=%s channel=%s] posted message to %s (ts=%s)", userID, channelID, args.ChannelID, ts)
		return fmt.Sprintf("Successfully posted to channel %s (ts=%s).", args.ChannelID, ts)

	case ToolSlackConversationsHistory:
		args, errMsg := parseToolArgs[struct {
			ChannelID string `json:"channel_id"`
			Oldest    string `json:"oldest"`
			Limit     int    `json:"limit"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.ChannelID) == "" {
			return "Error: 'channel_id' is required."
		}
		limit := args.Limit
		if limit <= 0 {
			limit = 20
		}
		if limit > 100 {
			limit = 100
		}
		msgs, err := h.slackClient.FetchChannelHistoryWindow(args.ChannelID, strings.TrimSpace(args.Oldest), limit)
		if err != nil {
			return fmt.Sprintf("Error fetching channel history for %s: %v", args.ChannelID, err)
		}
		if len(msgs) == 0 {
			return fmt.Sprintf("No messages found in channel %s after oldest=%q.", args.ChannelID, args.Oldest)
		}
		formatted := formatMessages(msgs)
		log.Printf("[user=%s channel=%s] fetched %d messages from %s (oldest=%q)", userID, channelID, len(msgs), args.ChannelID, args.Oldest)
		return fmt.Sprintf("Channel history (channel_id=%s, count=%d, oldest_requested=%q):\n\n%s", args.ChannelID, len(msgs), args.Oldest, formatted)

	case ToolSlackConversationsReplies:
		args, errMsg := parseToolArgs[struct {
			ChannelID string `json:"channel_id"`
			ThreadTS  string `json:"thread_ts"`
			Limit     int    `json:"limit"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.ChannelID) == "" || strings.TrimSpace(args.ThreadTS) == "" {
			return "Error: 'channel_id' and 'thread_ts' are required."
		}
		limit := args.Limit
		if limit <= 0 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}
		msgs, err := h.slackClient.FetchThreadReplies(args.ChannelID, strings.TrimSpace(args.ThreadTS), limit)
		if err != nil {
			return fmt.Sprintf("Error fetching thread replies for %s (thread_ts=%s): %v", args.ChannelID, args.ThreadTS, err)
		}
		if len(msgs) == 0 {
			return fmt.Sprintf("No messages found in thread %s/%s (the parent ts may be wrong or the thread has no replies).", args.ChannelID, args.ThreadTS)
		}
		formatted := formatMessages(msgs)
		log.Printf("[user=%s channel=%s] fetched %d thread messages from %s (thread_ts=%s)", userID, channelID, len(msgs), args.ChannelID, args.ThreadTS)
		return fmt.Sprintf("Thread replies (channel_id=%s, thread_ts=%s, count=%d, includes parent):\n\n%s", args.ChannelID, args.ThreadTS, len(msgs), formatted)

	case ToolUploadSnippet:
		args, errMsg := parseToolArgs[struct {
			Content   string `json:"content"`
			Filename  string `json:"filename"`
			Title     string `json:"title"`
			Filetype  string `json:"filetype"`
			ChannelID string `json:"channel_id"`
			ThreadTS  string `json:"thread_ts"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.Content) == "" {
			return preconditionErrf("Error: the 'content' argument is empty. You must pass the FULL file content as a string in the 'content' field of the same upload_snippet call (it is not generated for you). Re-issue the call with 'content' populated.")
		}
		if args.Filename == "" {
			return preconditionErrf("Error: the 'filename' argument is required (e.g. 'report.csv').")
		}
		if args.Filetype == "" {
			args.Filetype = "text"
		}
		targetChannel := h.resolveUploadChannel(channelID, args.ChannelID)
		if targetChannel == "" {
			return preconditionErrf("Error: no Slack channel to upload to. In a scheduled workflow there is no current channel — pass an explicit 'channel_id' (the same channel you post the report to).")
		}
		threadTS := h.resolveUploadThread(args.ThreadTS)
		fileID, err := h.slackClient.UploadFileSnippet(targetChannel, threadTS, args.Filename, args.Title, args.Content, args.Filetype)
		if err != nil {
			return fmt.Sprintf("Error uploading snippet: %v", err)
		}
		log.Printf("[user=%s channel=%s] uploaded file snippet %s (%s) to %s", userID, channelID, fileID, args.Filename, targetChannel)
		return fmt.Sprintf("Successfully uploaded snippet '%s' as %s to channel %s.", args.Title, args.Filename, targetChannel)

	case ToolUploadAggregateCSV:
		args, errMsg := parseToolArgs[struct {
			AggregateIDs []string `json:"aggregate_ids"`
			Filename     string   `json:"filename"`
			Title        string   `json:"title"`
			ChannelID    string   `json:"channel_id"`
			ThreadTS     string   `json:"thread_ts"`
			S3Bucket     string   `json:"s3_bucket"`
			S3Key        string   `json:"s3_key"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if len(args.AggregateIDs) == 0 {
			return preconditionErrf("Error: 'aggregate_ids' is required — pass the ids returned by your datadog_logs_aggregate calls (e.g. [\"agg_1\",\"agg_2\"]).")
		}
		if args.Filename == "" {
			return preconditionErrf("Error: 'filename' is required (e.g. 'report.csv').")
		}
		targetChannel := h.resolveUploadChannel(channelID, args.ChannelID)
		if targetChannel == "" {
			return preconditionErrf("Error: no Slack channel to upload to. In a scheduled workflow there is no current channel — pass an explicit 'channel_id' (the same channel you post the report to).")
		}
		buildErr, content := h.buildAggregateCSV(args.AggregateIDs)
		if buildErr != "" {
			return preconditionErrf("%s", buildErr)
		}
		threadTS := h.resolveUploadThread(args.ThreadTS)
		title := args.Title
		if title == "" {
			title = args.Filename
		}
		fileID, err := h.slackClient.UploadFileSnippet(targetChannel, threadTS, args.Filename, title, content, "csv")
		if err != nil {
			return fmt.Sprintf("Error uploading aggregate CSV: %v", err)
		}
		log.Printf("[user=%s channel=%s] uploaded aggregate CSV %s (%s, ids=%v, %d bytes) to %s", userID, channelID, fileID, args.Filename, args.AggregateIDs, len(content), targetChannel)
		result := fmt.Sprintf("Successfully uploaded '%s' as %s (%d bytes) to channel %s.", title, args.Filename, len(content), targetChannel)
		// Optional: archive the identical CSV bytes to S3 for durable storage
		// outside Slack's retention. The Slack upload above is the primary
		// deliverable — an S3 failure annotates the result but never fails the
		// tool, since the file is already in Slack by this point.
		if s3Bucket := strings.TrimSpace(args.S3Bucket); s3Bucket != "" {
			switch {
			case !h.canUseIntegration(integrationAWS) || h.awsClient == nil:
				result += " (S3 archive skipped: the AWS integration is not available to this agent.)"
			default:
				putRes, putErr := h.awsClient.S3PutObject(ctx, s3Bucket, args.S3Key, []byte(content), "text/csv")
				if putErr != nil {
					log.Printf("[user=%s channel=%s] aggregate CSV S3 archive failed (bucket=%s key=%s): %v", userID, channelID, s3Bucket, args.S3Key, putErr)
					result += fmt.Sprintf(" (S3 archive FAILED: %v — the Slack upload still succeeded.)", putErr)
				} else {
					uri := aws.S3URI(putRes.Bucket, putRes.Key)
					console := aws.S3ConsoleURL(putRes.Bucket, putRes.Key, putRes.Region)
					log.Printf("[user=%s channel=%s] archived aggregate CSV to %s (region=%s, %d bytes)", userID, channelID, uri, putRes.Region, putRes.Size)
					result += fmt.Sprintf(" Also archived to %s (region %s). Console link: %s", uri, putRes.Region, console)
				}
			}
		}
		return result

	case ToolFetchThreadContext:
		args, errMsg := parseToolArgs[struct {
			URL string `json:"url"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		threadChannelID, threadTS, err := ParseSlackThreadURL(args.URL)
		if err != nil {
			return fmt.Sprintf("Error parsing Slack thread URL: %v", err)
		}
		msgs, err := h.slackClient.FetchThreadReplies(threadChannelID, threadTS, 100)
		if err != nil {
			return fmt.Sprintf("Error fetching thread replies: %v", err)
		}
		if len(msgs) == 0 {
			return fmt.Sprintf("No messages found in thread (channel=%s, thread_ts=%s).", threadChannelID, threadTS)
		}
		formatted := formatMessages(msgs)
		log.Printf("[user=%s channel=%s] fetched thread context from %s (%d messages)", userID, channelID, args.URL, len(msgs))
		return fmt.Sprintf("Thread context (channel_id=%s, thread_ts=%s):\n\n%s", threadChannelID, threadTS, formatted)

	case ToolCreateJiraTicket:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			Project         string                 `json:"project"`
			Summary         string                 `json:"summary"`
			Description     string                 `json:"description"`
			IssueType       string                 `json:"issue_type"`
			Labels          []string               `json:"labels"`
			Assignee        string                 `json:"assignee"`
			AccountID       string                 `json:"account_id"`
			Team            string                 `json:"team"`
			UseActiveSprint bool                   `json:"use_active_sprint"`
			Fields          map[string]interface{} `json:"fields"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		// Append agent stamp to the description.
		stamp := fmt.Sprintf("\n\n---\nCreated by **%s** via Arbetern", h.agentID)
		if h.appURL != "" {
			stamp += fmt.Sprintf(" | %s/ui/", strings.TrimRight(h.appURL, "/"))
		}
		if h.currentChannelID != "" && h.currentAuditTS != "" {
			if permalink, err := h.slackClient.GetPermalink(h.currentChannelID, h.currentAuditTS); err == nil && permalink != "" {
				stamp += fmt.Sprintf(" | [Slack message](%s)", permalink)
			}
		}
		args.Description += stamp

		// Resolve assignee. Prefer an explicit account_id (skips fuzzy search
		// and any silent mismatches); fall back to name-based search otherwise.
		var assigneeID string
		if args.AccountID != "" {
			assigneeID = args.AccountID
			log.Printf("[user=%s channel=%s] using provided assignee account_id %s", userID, channelID, assigneeID)
		} else if args.Assignee != "" {
			project := args.Project
			users, err := h.jiraClient.SearchAssignableUsers(args.Assignee, project)
			if err != nil {
				log.Printf("[user=%s channel=%s] Jira user search failed for %q: %v", userID, channelID, args.Assignee, err)
			} else if len(users) > 0 {
				best, isGood := atlassian.BestUserMatch(users, args.Assignee)
				if isGood {
					assigneeID = best.AccountID
					log.Printf("[user=%s channel=%s] resolved assignee %q to user %s (%s)", userID, channelID, args.Assignee, best.DisplayName, assigneeID)
				} else {
					log.Printf("[user=%s channel=%s] user search for %q returned %d results but none matched well (top: %s)", userID, channelID, args.Assignee, len(users), users[0].DisplayName)
				}
			} else {
				log.Printf("[user=%s channel=%s] no Jira user found for %q", userID, channelID, args.Assignee)
			}
		}

		// Resolve team name independently.
		var teamFieldID string
		var teamID string
		var teamDisplayName string
		if args.Team != "" {
			fid, tid, dname, err := h.jiraClient.ResolveTeam(args.Team)
			if err != nil {
				log.Printf("[user=%s channel=%s] team resolution failed for %q: %v", userID, channelID, args.Team, err)
			} else {
				teamFieldID = fid
				teamID = tid
				teamDisplayName = dname
				log.Printf("[user=%s channel=%s] resolved %q to team %s (field: %s)", userID, channelID, args.Team, teamDisplayName, teamFieldID)
			}
		}

		// Attribute the Reporter to the human who made the request: an
		// interactive Slack command resolves via the Slack user id; a web-chat
		// turn behind the OAuth proxy resolves via the verified email. Headless
		// automation (scheduled workflows) has no requester, so the reporter is
		// left unset and Jira defaults it to the Arbetern service account.
		var reporterID, reporterName string
		switch {
		case !h.headless:
			reporterID, reporterName = h.resolveRequesterReporter(userID)
		case h.requesterEmail != "":
			reporterID, reporterName = h.resolveJiraReporterByEmail(h.requesterEmail)
		}
		if !h.headless || h.requesterEmail != "" {
			if reporterID != "" {
				log.Printf("[user=%s channel=%s] setting Jira reporter to requester %s (%s)", userID, channelID, reporterName, reporterID)
			} else {
				log.Printf("[user=%s channel=%s] could not resolve requester to a Jira account; reporter defaults to the Arbetern service account", userID, channelID)
			}
		}

		createInput := atlassian.CreateIssueInput{
			Project:     args.Project,
			Summary:     args.Summary,
			Description: args.Description,
			IssueType:   args.IssueType,
			Labels:      args.Labels,
			AssigneeID:  assigneeID,
			ReporterID:  reporterID,
			Fields:      args.Fields,
		}
		issue, err := h.jiraClient.CreateIssue(createInput)
		if err != nil && reporterID != "" && strings.Contains(strings.ToLower(err.Error()), "reporter") {
			// The service account likely lacks the "Modify Reporter" permission
			// (a separate Jira permission from Create Issues), so Jira rejected
			// the reporter field. Retry without it rather than failing the whole
			// request: the ticket is still created (reporter defaults to the
			// service account) and the gap is logged so an operator can grant
			// the permission to restore human attribution.
			log.Printf("[user=%s channel=%s] Jira rejected reporter on create (%v); retrying without reporter — grant the service account 'Modify Reporter' to attribute tickets to the requester", userID, channelID, err)
			createInput.ReporterID = ""
			reporterName = ""
			issue, err = h.jiraClient.CreateIssue(createInput)
		}
		if err != nil {
			return fmt.Sprintf("Error creating Jira ticket: %v", err)
		}

		// Set team if resolved (update after creation since team is a custom field).
		if teamFieldID != "" && teamID != "" {
			if err := h.jiraClient.SetTeamField(issue.Key, teamFieldID, teamID); err != nil {
				log.Printf("[user=%s channel=%s] failed to set team %s on %s: %v", userID, channelID, teamDisplayName, issue.Key, err)
			} else {
				log.Printf("[user=%s channel=%s] set team %s on %s", userID, channelID, teamDisplayName, issue.Key)
			}
		}

		// Drop into the active sprint if requested. Done post-create because
		// the Sprint custom field id has to be discovered first and Jira
		// Cloud rejects sprint writes inside the create payload on some
		// instances; the update path is the reliable one.
		if args.UseActiveSprint {
			sprint, err := h.jiraClient.GetActiveSprintForProject(args.Project)
			switch {
			case err != nil:
				log.Printf("[user=%s channel=%s] active sprint lookup failed for %s: %v", userID, channelID, issue.Key, err)
			case sprint == nil:
				log.Printf("[user=%s channel=%s] no active sprint found for project of %s", userID, channelID, issue.Key)
			default:
				if err := h.jiraClient.SetSprintField(issue.Key, sprint.ID); err != nil {
					log.Printf("[user=%s channel=%s] failed to set sprint %d on %s: %v", userID, channelID, sprint.ID, issue.Key, err)
				} else {
					log.Printf("[user=%s channel=%s] added %s to active sprint %q (id %d)", userID, channelID, issue.Key, sprint.Name, sprint.ID)
				}
			}
		}

		log.Printf("[user=%s channel=%s] created Jira ticket %s: %s", userID, channelID, issue.Key, issue.Browse)
		result := fmt.Sprintf("Jira ticket created: *%s* — %s\nSummary: %s", issue.Key, issue.Browse, args.Summary)
		if reporterName != "" {
			result += fmt.Sprintf("\nReporter: %s (the requesting user)", reporterName)
		}
		return result

	case ToolListJiraProjects:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		projects, err := h.jiraClient.ListProjects()
		if err != nil {
			return fmt.Sprintf("Error listing Jira projects: %v", err)
		}
		if len(projects) == 0 {
			return "No Jira projects found."
		}
		log.Printf("[user=%s channel=%s] listed %d Jira projects", userID, channelID, len(projects))
		return fmt.Sprintf("Jira projects (%d):\n%s", len(projects), strings.Join(projects, "\n"))

	case ToolSearchJiraIssues:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			JQL        string `json:"jql"`
			MaxResults int    `json:"max_results"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		issues, err := h.jiraClient.SearchIssuesJQL(args.JQL, args.MaxResults)
		if err != nil {
			return fmt.Sprintf("Error searching Jira issues: %v", err)
		}
		if len(issues) == 0 {
			return fmt.Sprintf("No issues found for JQL: %s", args.JQL)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d issues:\n\n", len(issues))
		for _, i := range issues {
			fmt.Fprintf(&sb, "• *%s* — %s\n  Status: %s | Type: %s | Priority: %s\n  Assignee: %s", i.Key, i.Summary, i.Status, i.IssueType, i.Priority, i.Assignee)
			if i.Team != "" {
				fmt.Fprintf(&sb, " | Team: %s", i.Team)
			}
			if i.Sprint != "" {
				fmt.Fprintf(&sb, " | Sprint: %s", i.Sprint)
			}
			fmt.Fprintf(&sb, " | Updated: %s\n  URL: %s\n", i.Updated, i.Browse)
			if i.Description != "" {
				desc := i.Description
				if len(desc) > 500 {
					desc = desc[:500] + "... (truncated)"
				}
				fmt.Fprintf(&sb, "  Description: %s\n", desc)
			}
			sb.WriteString("\n")
		}
		log.Printf("[user=%s channel=%s] searched Jira issues with JQL, found %d", userID, channelID, len(issues))
		return sb.String()

	case ToolGetJiraIssue:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			IssueKey string `json:"issue_key"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		issue, err := h.jiraClient.GetIssue(args.IssueKey)
		if err != nil {
			return fmt.Sprintf("Error getting Jira issue: %v", err)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "*%s* — %s\n", issue.Key, issue.Summary)
		fmt.Fprintf(&sb, "Status: %s | Type: %s | Priority: %s\n", issue.Status, issue.IssueType, issue.Priority)
		fmt.Fprintf(&sb, "Assignee: %s | Reporter: %s\n", issue.Assignee, issue.Reporter)
		if issue.Team != "" {
			fmt.Fprintf(&sb, "Team: %s\n", issue.Team)
		}
		if issue.Sprint != "" {
			fmt.Fprintf(&sb, "Sprint: %s\n", issue.Sprint)
		}
		fmt.Fprintf(&sb, "Updated: %s\n", issue.Updated)
		if len(issue.Labels) > 0 {
			fmt.Fprintf(&sb, "Labels: %s\n", strings.Join(issue.Labels, ", "))
		}
		fmt.Fprintf(&sb, "URL: %s\n", issue.Browse)
		if issue.Description != "" {
			fmt.Fprintf(&sb, "\nDescription:\n%s\n", issue.Description)
		} else {
			fmt.Fprintf(&sb, "\nDescription: (empty)\n")
		}
		log.Printf("[user=%s channel=%s] fetched Jira issue %s", userID, channelID, args.IssueKey)
		return sb.String()

	case ToolGetJiraLabelAuthor:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			IssueKey string `json:"issue_key"`
			Label    string `json:"label"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		changes, err := h.jiraClient.GetLabelChangeHistory(args.IssueKey, args.Label)
		if err != nil {
			return fmt.Sprintf("Error getting Jira label history: %v", err)
		}
		label := strings.TrimSpace(args.Label)
		if len(changes) == 0 {
			if label != "" {
				return fmt.Sprintf("No changelog entry adds or removes label %q on %s. The label may predate the available history, or it was set at creation without a recorded change. Do NOT attribute it to the workflow creator or the issue reporter/assignee — leave the author unresolved.", label, args.IssueKey)
			}
			return fmt.Sprintf("No label changes are recorded in the changelog of %s. Do NOT guess an author from the reporter, assignee, or workflow creator — leave it unresolved.", args.IssueKey)
		}
		var sb strings.Builder
		if label != "" {
			fmt.Fprintf(&sb, "Label-change history for %q on %s (oldest first):\n", label, args.IssueKey)
		} else {
			fmt.Fprintf(&sb, "Label-change history for %s (oldest first):\n", args.IssueKey)
		}
		var lastAdd *atlassian.LabelChange
		for i := range changes {
			ch := changes[i]
			emailPart := ""
			if ch.Email != "" {
				emailPart = ", email=" + ch.Email
			}
			fmt.Fprintf(&sb, "- %s %q by %s (accountId=%s%s) at %s\n",
				ch.Action, ch.Label, ch.Author, ch.AccountID, emailPart, ch.Created)
			if ch.Action == "added" {
				lastAdd = &changes[i]
			}
		}
		if lastAdd != nil {
			emailPart := ""
			if lastAdd.Email != "" {
				emailPart = ", email=" + lastAdd.Email
			}
			fmt.Fprintf(&sb, "\nMost recent ADD: %s (accountId=%s%s) at %s. Attribute the label to THIS person. If this person is the arbetern automation/bot account, fall back to the most recent ADD by a human instead.\n",
				lastAdd.Author, lastAdd.AccountID, emailPart, lastAdd.Created)
		} else {
			fmt.Fprintf(&sb, "\nNo ADD entry found (only removals). Leave the author unresolved rather than guessing.\n")
		}
		log.Printf("[user=%s channel=%s] fetched Jira label history for %s (%d changes)", userID, channelID, args.IssueKey, len(changes))
		return sb.String()

	case ToolUpdateJiraIssue:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			IssueKey    string `json:"issue_key"`
			Summary     string `json:"summary"`
			Description string `json:"description"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.Summary == "" && args.Description == "" {
			return "Error: at least one of summary or description must be provided."
		}
		// Update summary if provided.
		if args.Summary != "" {
			if err := h.jiraClient.UpdateIssueFields(args.IssueKey, map[string]interface{}{"summary": args.Summary}); err != nil {
				return fmt.Sprintf("Error updating summary: %v", err)
			}
		}
		// Update description if provided (using ADF format).
		if args.Description != "" {
			if err := h.jiraClient.UpdateIssueDescription(args.IssueKey, args.Description); err != nil {
				return fmt.Sprintf("Error updating description: %v", err)
			}
		}
		updated := []string{}
		if args.Summary != "" {
			updated = append(updated, "summary")
		}
		if args.Description != "" {
			updated = append(updated, "description")
		}
		log.Printf("[user=%s channel=%s] updated Jira issue %s (%s)", userID, channelID, args.IssueKey, strings.Join(updated, ", "))
		return fmt.Sprintf("Successfully updated %s: %s", args.IssueKey, strings.Join(updated, " and "))

	case ToolAssignJiraActiveSprint:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			IssueKey string `json:"issue_key"`
			Project  string `json:"project"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		issueKey := strings.TrimSpace(args.IssueKey)
		if issueKey == "" {
			return "Error: issue_key is required."
		}
		project := strings.TrimSpace(args.Project)
		if project == "" {
			if i := strings.Index(issueKey, "-"); i > 0 {
				project = issueKey[:i]
			}
		}
		if project == "" {
			return "Error: could not determine project from issue_key; pass project explicitly."
		}
		sprint, err := h.jiraClient.GetActiveSprintForProject(project)
		if err != nil {
			return fmt.Sprintf("Error resolving active sprint for project %s: %v", project, err)
		}
		if sprint == nil {
			return fmt.Sprintf("No active sprint on project %s — nothing to assign.", project)
		}
		// Skip if the issue is already in the named active sprint.
		if issue, err := h.jiraClient.GetIssue(issueKey); err == nil && strings.EqualFold(issue.Sprint, sprint.Name) {
			return fmt.Sprintf("%s is already in active sprint %q (id %d) — no change.", issueKey, sprint.Name, sprint.ID)
		}
		if err := h.jiraClient.SetSprintField(issueKey, sprint.ID); err != nil {
			return fmt.Sprintf("Error assigning %s to active sprint %q (id %d): %v", issueKey, sprint.Name, sprint.ID, err)
		}
		log.Printf("[user=%s channel=%s] assigned %s to active sprint %q (id %d) on project %s", userID, channelID, issueKey, sprint.Name, sprint.ID, project)
		return fmt.Sprintf("Assigned %s to active sprint %q (id %d).", issueKey, sprint.Name, sprint.ID)

	case ToolAssignJiraTeam:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			IssueKey string `json:"issue_key"`
			TeamID   string `json:"team_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		issueKey := strings.TrimSpace(args.IssueKey)
		teamID := strings.TrimSpace(args.TeamID)
		if issueKey == "" || teamID == "" {
			return "Error: issue_key and team_id are both required."
		}
		fields, err := h.jiraClient.FindTeamFields()
		if err != nil || len(fields) == 0 {
			return fmt.Sprintf("Error discovering Team field on Jira instance: %v", err)
		}
		fieldID := fields[0].ID
		if err := h.jiraClient.SetTeamField(issueKey, fieldID, teamID); err != nil {
			return fmt.Sprintf("Error setting team %s on %s: %v", teamID, issueKey, err)
		}
		log.Printf("[user=%s channel=%s] assigned %s to team %s (field %s)", userID, channelID, issueKey, teamID, fieldID)
		return fmt.Sprintf("Assigned %s to team %s.", issueKey, teamID)

	case ToolAddJiraComment:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			IssueKey string `json:"issue_key"`
			Body     string `json:"body"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		comment, err := h.jiraClient.AddComment(args.IssueKey, args.Body)
		if err != nil {
			return fmt.Sprintf("Error adding comment to %s: %v", args.IssueKey, err)
		}
		log.Printf("[user=%s channel=%s] added Jira comment %s on %s", userID, channelID, comment.ID, args.IssueKey)
		return fmt.Sprintf("Comment added to %s (id %s) by %s.", args.IssueKey, comment.ID, comment.Author.DisplayName)

	case ToolListJiraComments:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			IssueKey string `json:"issue_key"`
			Limit    int    `json:"limit"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		comments, err := h.jiraClient.ListComments(args.IssueKey, args.Limit)
		if err != nil {
			return fmt.Sprintf("Error listing comments on %s: %v", args.IssueKey, err)
		}
		if len(comments) == 0 {
			return fmt.Sprintf("No comments on %s.", args.IssueKey)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Comments on %s (newest first, %d):\n", args.IssueKey, len(comments))
		for _, c := range comments {
			body := strings.TrimSpace(c.Body)
			if len(body) > 800 {
				body = body[:800] + "…"
			}
			fmt.Fprintf(&sb, "\n— %s (%s) [%s]\n%s\n", c.Author.DisplayName, c.Created, c.ID, body)
		}
		log.Printf("[user=%s channel=%s] listed %d comments on %s", userID, channelID, len(comments), args.IssueKey)
		return sb.String()

	case ToolLinkJiraIssues:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			InwardKey  string `json:"inward_key"`
			OutwardKey string `json:"outward_key"`
			LinkType   string `json:"link_type"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if err := h.jiraClient.LinkIssues(args.InwardKey, args.OutwardKey, args.LinkType); err != nil {
			return fmt.Sprintf("Error linking %s → %s as %q: %v", args.InwardKey, args.OutwardKey, args.LinkType, err)
		}
		log.Printf("[user=%s channel=%s] linked %s -> %s as %q", userID, channelID, args.InwardKey, args.OutwardKey, args.LinkType)
		return fmt.Sprintf("Linked %s → %s as %q.", args.InwardKey, args.OutwardKey, args.LinkType)

	case ToolGetSlackUserInfo:
		args, errMsg := parseToolArgs[struct {
			UserID string `json:"user_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		user, err := h.slackClient.GetUserInfo(args.UserID)
		if err != nil {
			return fmt.Sprintf("Error getting user info: %v", err)
		}
		return fmt.Sprintf("Slack User Info:\n  User ID: %s\n  Real Name: %s\n  Display Name: %s\n  Email: %s\n  Title: %s",
			user.ID, user.RealName, user.Profile.DisplayName, user.Profile.Email, user.Profile.Title)

	case ToolLookupSlackUser:
		args, errMsg := parseToolArgs[struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		email := strings.TrimSpace(args.Email)
		name := strings.TrimSpace(args.Name)
		if email == "" && name == "" {
			return "Error: provide at least one of 'email' or 'name'."
		}
		// Email is the reliable key — try it first.
		if email != "" {
			if user, err := h.slackClient.GetUserByEmail(email); err == nil && user != nil {
				log.Printf("[user=%s channel=%s] resolved Slack user by email → %s", userID, channelID, user.ID)
				return fmt.Sprintf("Resolved Slack user by email.\n  User ID: %s\n  Mention: <@%s>\n  Real Name: %s\n  Display Name: %s\n\nUse the Mention token <@%s> verbatim in a Slack message to actually ping this person.",
					user.ID, user.ID, user.RealName, user.Profile.DisplayName, user.ID)
			} else if err != nil {
				log.Printf("[user=%s channel=%s] Slack lookup by email failed: %v", userID, channelID, err)
			}
		}
		// Fallback: conservative exact name match.
		if name != "" {
			user, matched, err := h.slackClient.GetUserByName(name)
			if err != nil {
				return fmt.Sprintf("Error looking up Slack user by name: %v", err)
			}
			if matched && user != nil {
				log.Printf("[user=%s channel=%s] resolved Slack user by name %q → %s", userID, channelID, name, user.ID)
				return fmt.Sprintf("Resolved Slack user by name (unique match).\n  User ID: %s\n  Mention: <@%s>\n  Real Name: %s\n  Display Name: %s\n\nUse the Mention token <@%s> verbatim in a Slack message to actually ping this person.",
					user.ID, user.ID, user.RealName, user.Profile.DisplayName, user.ID)
			}
		}
		hint := ""
		if email == "" {
			hint = " No email was provided — the source system may hide it; a name-only match is only possible when exactly one Slack user has that name."
		}
		return fmt.Sprintf("Could not uniquely resolve a Slack user from the given email/name.%s Do NOT invent a Slack ID. Fall back to plain text \"@%s\" (which does not ping) for attribution.", hint, name)

	case ToolResolveJiraTeam:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			TeamName string `json:"team_name"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		// First discover the JQL clause name for the Team field.
		fields, err := h.jiraClient.FindTeamFields()
		if err != nil {
			return fmt.Sprintf("Error discovering Team field: %v", err)
		}
		jqlClause := fields[0].JQLName
		// Then resolve the team name to its UUID.
		_, teamID, displayName, err := h.jiraClient.ResolveTeam(args.TeamName)
		if err != nil {
			return fmt.Sprintf("Error resolving team %q: %v. Try a different team name spelling.", args.TeamName, err)
		}
		log.Printf("[user=%s channel=%s] resolved Jira team %q → %s (clause: %s)", userID, channelID, args.TeamName, teamID, jqlClause)
		return fmt.Sprintf("Team resolved:\n  Display Name: %s\n  Team UUID: %s\n  JQL Clause: %s\n\nUse in JQL: \"%s\" = \"%s\"\nExample: \"%s\" = \"%s\" AND status = \"In Progress\" ORDER BY priority DESC", displayName, teamID, jqlClause, jqlClause, teamID, jqlClause, teamID)

	case ToolResolveJiraUser:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}

		// Multi-strategy search: email first (most reliable), then full name, then individual name parts.
		type attempt struct {
			label string
			query string
		}
		var attempts []attempt
		if args.Email != "" {
			attempts = append(attempts, attempt{"email", args.Email})
		}
		if args.Name != "" {
			attempts = append(attempts, attempt{"full name", args.Name})
			// Also try individual name parts (first name, last name) since Jira's
			// /user/search often matches prefixes, and "Jane Doe" as a single
			// query may fail while "Jane" succeeds.
			parts := strings.Fields(args.Name)
			if len(parts) > 1 {
				for _, p := range parts {
					attempts = append(attempts, attempt{"name part", p})
				}
			}
		}

		var users []atlassian.JiraUser
		var matchLabel string
		for _, a := range attempts {
			result, err := h.jiraClient.SearchUsersGeneral(a.query)
			if err != nil {
				log.Printf("[user=%s channel=%s] Jira user search by %s (%q) failed: %v", userID, channelID, a.label, a.query, err)
				continue
			}
			if len(result) > 0 {
				users = result
				matchLabel = a.label
				log.Printf("[user=%s channel=%s] Jira user search by %s (%q) returned %d result(s)", userID, channelID, a.label, a.query, len(result))
				break
			}
			log.Printf("[user=%s channel=%s] Jira user search by %s (%q) returned 0 results, trying next strategy", userID, channelID, a.label, a.query)
		}

		if len(users) == 0 {
			// Final fallback: reverse-lookup via project issues. This works even when
			// the service account lacks "Browse users and groups" global permission,
			// because the issue search endpoint returns assignee accountIds.
			log.Printf("[user=%s channel=%s] all /user/search strategies failed, trying issue-based reverse lookup for %q", userID, channelID, args.Name)
			issueUsers, err := h.jiraClient.ResolveUserViaIssues(args.Name)
			if err != nil {
				log.Printf("[user=%s channel=%s] issue-based user lookup failed: %v", userID, channelID, err)
			} else if len(issueUsers) > 0 {
				users = issueUsers
				matchLabel = "issue assignee reverse lookup"
				log.Printf("[user=%s channel=%s] issue-based reverse lookup found %d match(es) for %q", userID, channelID, len(users), args.Name)
			}
		}

		if len(users) == 0 {
			return fmt.Sprintf("No Jira users found matching name=%q email=%q after trying all search strategies (user search + issue reverse lookup). Verify the user exists in Jira and has issues assigned in project.", args.Name, args.Email)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d Jira user(s) (matched by %s):\n", len(users), matchLabel)
		for i, u := range users {
			if i >= 5 {
				fmt.Fprintf(&sb, "  ... and %d more\n", len(users)-5)
				break
			}
			fmt.Fprintf(&sb, "  • %s (accountId: %s, active: %v)\n", u.DisplayName, u.AccountID, u.Active)
		}
		fmt.Fprintf(&sb, "\nUse the accountId in JQL queries like: assignee = \"%s\"\n", users[0].AccountID)
		log.Printf("[user=%s channel=%s] resolved Jira user %q -> %s (%s) via %s", userID, channelID, args.Name, users[0].DisplayName, users[0].AccountID, matchLabel)
		return sb.String()

	// ---- Dashboard & Filter tools ----

	case ToolGetJiraDashboard:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			DashboardID string `json:"dashboard_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.DashboardID == "" {
			return "Error: dashboard_id is required."
		}
		dash, err := h.jiraClient.GetDashboard(args.DashboardID)
		if err != nil {
			return fmt.Sprintf("Error fetching dashboard: %v", err)
		}
		gadgets, gadgetErr := h.jiraClient.GetDashboardGadgets(args.DashboardID)

		var sb2 strings.Builder
		fmt.Fprintf(&sb2, "*Dashboard: %s* (ID: %s)\n", dash.Name, dash.ID)
		if dash.Description != "" {
			fmt.Fprintf(&sb2, "Description: %s\n", dash.Description)
		}
		fmt.Fprintf(&sb2, "Owner: %s\n", dash.Owner.DisplayName)
		if dash.View != "" {
			fmt.Fprintf(&sb2, "URL: %s\n", dash.View)
		}

		if gadgetErr != nil {
			fmt.Fprintf(&sb2, "\nGadgets: could not fetch (%v)\n", gadgetErr)
		} else if len(gadgets) == 0 {
			fmt.Fprintf(&sb2, "\nGadgets: none\n")
		} else {
			fmt.Fprintf(&sb2, "\nGadgets (%d):\n", len(gadgets))
			for _, g := range gadgets {
				title := g.Title
				if title == "" {
					title = g.ModuleKey
				}
				if title == "" {
					title = "(untitled)"
				}
				fmt.Fprintf(&sb2, "  • %s (row %d, col %d)", title, g.Position.Row, g.Position.Column)
				if g.Color != "" {
					fmt.Fprintf(&sb2, " [%s]", g.Color)
				}
				sb2.WriteString("\n")
			}
		}
		log.Printf("[user=%s channel=%s] fetched Jira dashboard %s (%s, %d gadgets)", userID, channelID, args.DashboardID, dash.Name, len(gadgets))
		return sb2.String()

	case ToolGetJiraFilter:
		if errMsg := requireReady("Jira", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			FilterID   string `json:"filter_id"`
			RunJQL     *bool  `json:"run_jql"`
			MaxResults int    `json:"max_results"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.FilterID == "" {
			return "Error: filter_id is required."
		}
		filter, err := h.jiraClient.GetFilter(args.FilterID)
		if err != nil {
			return fmt.Sprintf("Error fetching filter: %v", err)
		}

		var sb3 strings.Builder
		fmt.Fprintf(&sb3, "*Filter: %s* (ID: %s)\n", filter.Name, filter.ID)
		if filter.Description != "" {
			fmt.Fprintf(&sb3, "Description: %s\n", filter.Description)
		}
		fmt.Fprintf(&sb3, "Owner: %s\n", filter.Owner.DisplayName)
		fmt.Fprintf(&sb3, "JQL: `%s`\n", filter.JQL)
		if filter.ViewURL != "" {
			fmt.Fprintf(&sb3, "URL: %s\n", filter.ViewURL)
		}

		// Default run_jql to true.
		runJQL := args.RunJQL == nil || *args.RunJQL
		if runJQL && filter.JQL != "" {
			maxResults := args.MaxResults
			if maxResults <= 0 {
				maxResults = 20
			}
			if maxResults > 50 {
				maxResults = 50
			}
			issues, searchErr := h.jiraClient.SearchIssuesJQL(filter.JQL, maxResults)
			if searchErr != nil {
				fmt.Fprintf(&sb3, "\nFilter JQL execution failed: %v\n", searchErr)
			} else if len(issues) == 0 {
				fmt.Fprintf(&sb3, "\nFilter JQL returned 0 issues.\n")
			} else {
				fmt.Fprintf(&sb3, "\nFilter results (%d issues):\n\n", len(issues))
				for _, i := range issues {
					fmt.Fprintf(&sb3, "• *%s* — %s\n  Status: %s | Type: %s | Priority: %s\n  Assignee: %s", i.Key, i.Summary, i.Status, i.IssueType, i.Priority, i.Assignee)
					if i.Team != "" {
						fmt.Fprintf(&sb3, " | Team: %s", i.Team)
					}
					if i.Sprint != "" {
						fmt.Fprintf(&sb3, " | Sprint: %s", i.Sprint)
					}
					fmt.Fprintf(&sb3, " | Updated: %s\n  URL: %s\n\n", i.Updated, i.Browse)
				}
			}
		}
		log.Printf("[user=%s channel=%s] fetched Jira filter %s (%s, jql=%q)", userID, channelID, args.FilterID, filter.Name, filter.JQL)
		return sb3.String()

	// ---- Confluence tools ----

	case ToolSearchConfluencePages:
		if errMsg := requireReady("Atlassian", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			CQL   string `json:"cql"`
			Limit int    `json:"limit"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.CQL == "" {
			return "Error: cql is required."
		}
		if args.Limit <= 0 {
			args.Limit = 10
		}
		results, err := h.jiraClient.SearchConfluencePages(args.CQL, args.Limit)
		if err != nil {
			return fmt.Sprintf("Error searching Confluence: %v", err)
		}
		if len(results) == 0 {
			return fmt.Sprintf("No Confluence pages found matching CQL: %s", args.CQL)
		}
		log.Printf("[user=%s channel=%s] searched Confluence with CQL %q (%d results)", userID, channelID, args.CQL, len(results))
		var sb2 strings.Builder
		fmt.Fprintf(&sb2, "Found %d Confluence page(s):\n", len(results))
		for _, r := range results {
			link := r.WebURL
			if link == "" {
				link = "(no link)"
			}
			fmt.Fprintf(&sb2, "  • [%s] %s (id: %s) — %s\n", r.Type, r.Title, r.ID, link)
		}
		return sb2.String()

	case ToolGetConfluencePage:
		if errMsg := requireReady("Atlassian", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			PageID string `json:"page_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.PageID == "" {
			return "Error: page_id is required."
		}
		// Resolve URL or tiny link to numeric page ID.
		pageID, err := atlassian.ResolveConfluencePageID(args.PageID)
		if err != nil {
			return fmt.Sprintf("Error resolving Confluence page: %v", err)
		}
		page, err := h.jiraClient.GetConfluencePage(pageID)
		if err != nil {
			return fmt.Sprintf("Error fetching Confluence page: %v", err)
		}
		body := page.Body
		if len(body) > 12000 {
			body = body[:12000] + "\n... (truncated — page content is longer than shown)"
		}
		link := page.WebURL
		if link == "" {
			link = "(no link)"
		}
		log.Printf("[user=%s channel=%s] fetched Confluence page %s (%q)", userID, channelID, page.ID, page.Title)
		return fmt.Sprintf("Page: %s (id: %s, v%d)\nLink: %s\n\n%s", page.Title, page.ID, page.Version, link, body)

	case ToolListConfluenceSpaces:
		if errMsg := requireReady("Atlassian", h.jiraClient); errMsg != "" {
			return errMsg
		}
		spaces, err := h.jiraClient.ListConfluenceSpaces()
		if err != nil {
			return fmt.Sprintf("Error listing Confluence spaces: %v", err)
		}
		if len(spaces) == 0 {
			return "No Confluence spaces found."
		}
		log.Printf("[user=%s channel=%s] listed %d Confluence spaces", userID, channelID, len(spaces))
		var sb3 strings.Builder
		fmt.Fprintf(&sb3, "Confluence spaces (%d):\n", len(spaces))
		for _, s := range spaces {
			fmt.Fprintf(&sb3, "  • %s (key: %s, type: %s)\n", s.Name, s.Key, s.Type)
		}
		return sb3.String()

	case ToolCreateConfluencePage:
		if errMsg := requireReady("Atlassian", h.jiraClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			SpaceKey string `json:"space_key"`
			Title    string `json:"title"`
			Body     string `json:"body"`
			ParentID string `json:"parent_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.SpaceKey == "" || args.Title == "" {
			return "Error: space_key and title are required."
		}
		result, err := h.jiraClient.CreateConfluencePage(atlassian.CreateConfluencePageInput{
			SpaceKey: args.SpaceKey,
			Title:    args.Title,
			Body:     args.Body,
			ParentID: args.ParentID,
		})
		if err != nil {
			return fmt.Sprintf("Error creating Confluence page: %v", err)
		}
		log.Printf("[user=%s channel=%s] created Confluence page %s (%q) in space %s", userID, channelID, result.ID, result.Title, args.SpaceKey)
		return fmt.Sprintf("Confluence page created: *%s* (id: %s)\n%s", result.Title, result.ID, result.WebURL)

	case ToolLookupCVE:
		if h.nvdClient == nil {
			return "Error: NVD integration is not configured."
		}
		args, errMsg := parseToolArgs[struct {
			CVEID string `json:"cve_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		args.CVEID = strings.TrimSpace(strings.ToUpper(args.CVEID))
		if args.CVEID == "" {
			return "Error: cve_id is required."
		}
		cve, err := h.nvdClient.LookupCVE(ctx, args.CVEID)
		if err != nil {
			return fmt.Sprintf("Error looking up %s: %v", args.CVEID, err)
		}
		log.Printf("[user=%s channel=%s] looked up CVE %s from NVD", userID, channelID, args.CVEID)
		return nvd.FormatCVE(cve)

	case ToolSearchCVE:
		if h.nvdClient == nil {
			return "Error: NVD integration is not configured."
		}
		args, errMsg := parseToolArgs[struct {
			Keyword        string `json:"keyword"`
			ResultsPerPage int    `json:"results_per_page"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.Keyword == "" {
			return "Error: keyword is required."
		}
		items, total, err := h.nvdClient.SearchCVE(ctx, args.Keyword, args.ResultsPerPage)
		if err != nil {
			return fmt.Sprintf("Error searching NVD: %v", err)
		}
		if len(items) == 0 {
			return fmt.Sprintf("No CVEs found matching '%s'.", args.Keyword)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d CVEs matching '%s' (showing %d):\n\n", total, args.Keyword, len(items))
		for _, item := range items {
			sb.WriteString(nvd.FormatCVE(&item))
			sb.WriteString("\n---\n")
		}
		log.Printf("[user=%s channel=%s] searched NVD for '%s' (%d results)", userID, channelID, args.Keyword, total)
		return sb.String()

	case ToolSalesforceQuery:
		if errMsg := requireReady("Salesforce", h.sfClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			SOQL string `json:"soql"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.SOQL == "" {
			return "Error: soql is required."
		}
		log.Printf("[user=%s channel=%s] executing SOQL: %s", userID, channelID, args.SOQL)
		result, err := h.sfClient.Query(args.SOQL)
		if err != nil {
			return fmt.Sprintf("Error executing SOQL query: %v", err)
		}
		if len(result.Records) == 0 {
			return fmt.Sprintf("No records found for query: %s", args.SOQL)
		}
		// Auto-detect object type and format accordingly.
		soqlUpper := strings.ToUpper(args.SOQL)
		switch {
		case strings.Contains(soqlUpper, "FROM OPPORTUNITY"):
			opps := salesforce.ParseOpportunities(result.Records)
			log.Printf("[user=%s channel=%s] SOQL returned %d opportunities", userID, channelID, len(opps))
			return salesforce.FormatOpportunities(opps)
		case strings.Contains(soqlUpper, "FROM ACCOUNT"):
			accounts := salesforce.ParseAccounts(result.Records)
			log.Printf("[user=%s channel=%s] SOQL returned %d accounts", userID, channelID, len(accounts))
			return salesforce.FormatAccounts(accounts)
		case strings.Contains(soqlUpper, "FROM CONTACT"):
			contacts := salesforce.ParseContacts(result.Records)
			log.Printf("[user=%s channel=%s] SOQL returned %d contacts", userID, channelID, len(contacts))
			return salesforce.FormatContacts(contacts)
		default:
			// Generic JSON output for other objects.
			raw, _ := json.MarshalIndent(result.Records, "", "  ")
			output := string(raw)
			if len(output) > 8000 {
				output = output[:8000] + "\n... (truncated)"
			}
			log.Printf("[user=%s channel=%s] SOQL returned %d records (generic)", userID, channelID, result.TotalSize)
			return fmt.Sprintf("Query returned %d record(s):\n```\n%s\n```", result.TotalSize, output)
		}

	case ToolSalesforceDescribe:
		if errMsg := requireReady("Salesforce", h.sfClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			ObjectName string `json:"object_name"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.ObjectName == "" {
			return "Error: object_name is required."
		}
		desc, err := h.sfClient.Describe(args.ObjectName)
		if err != nil {
			return fmt.Sprintf("Error describing %s: %v", args.ObjectName, err)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "*%s* (%s) — %d fields:\n\n", desc.Label, desc.Name, len(desc.Fields))
		// Show only the most useful fields (skip internal/system fields).
		shown := 0
		for _, f := range desc.Fields {
			if shown >= 60 {
				fmt.Fprintf(&sb, "\n... and %d more fields (truncated)", len(desc.Fields)-shown)
				break
			}
			fmt.Fprintf(&sb, "• `%s` — %s (%s)\n", f.Name, f.Label, f.Type)
			shown++
		}
		log.Printf("[user=%s channel=%s] described Salesforce object %s (%d fields)", userID, channelID, args.ObjectName, len(desc.Fields))
		return sb.String()

	// ---- Chorus tools ----

	case ToolChorusListConversations:
		if errMsg := requireReady("Chorus", h.chorusClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			MinDate              string  `json:"min_date"`
			MaxDate              string  `json:"max_date"`
			MinDuration          float64 `json:"min_duration"`
			MaxDuration          float64 `json:"max_duration"`
			EngagementType       string  `json:"engagement_type"`
			ContentType          string  `json:"content_type"`
			ParticipantsEmail    string  `json:"participants_email"`
			UserID               string  `json:"user_id"`
			TeamID               string  `json:"team_id"`
			EngagementID         string  `json:"engagement_id"`
			WithTrackers         bool    `json:"with_trackers"`
			DispositionConnected bool    `json:"disposition_connected"`
			DispositionVoicemail bool    `json:"disposition_voicemail"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		engagements, err := h.chorusClient.ListEngagements(chorus.EngagementFilter{
			MinDate:              args.MinDate,
			MaxDate:              args.MaxDate,
			MinDuration:          args.MinDuration,
			MaxDuration:          args.MaxDuration,
			EngagementType:       args.EngagementType,
			ContentType:          args.ContentType,
			ParticipantsEmail:    args.ParticipantsEmail,
			UserID:               args.UserID,
			TeamID:               args.TeamID,
			EngagementID:         args.EngagementID,
			WithTrackers:         args.WithTrackers,
			DispositionConnected: args.DispositionConnected,
			DispositionVoicemail: args.DispositionVoicemail,
		})
		if err != nil {
			return fmt.Sprintf("Error listing Chorus conversations: %v", err)
		}
		if len(engagements) == 0 {
			return "No Chorus conversations found matching the filter."
		}
		log.Printf("[user=%s channel=%s] listed %d Chorus conversations", userID, channelID, len(engagements))
		return chorus.FormatEngagements(engagements)

	case ToolChorusGetConversation:
		if errMsg := requireReady("Chorus", h.chorusClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			ConversationID string `json:"conversation_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.ConversationID == "" {
			return "Error: conversation_id is required."
		}
		conv, err := h.chorusClient.GetConversation(args.ConversationID)
		if err != nil {
			return fmt.Sprintf("Error fetching Chorus conversation: %v", err)
		}
		log.Printf("[user=%s channel=%s] fetched Chorus conversation %s (%q)", userID, channelID, args.ConversationID, conv.Attributes.Name)
		return chorus.FormatConversation(conv)

	case ToolChorusCreateSalesQualification:
		if errMsg := requireReady("Chorus", h.chorusClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			RecordingID string `json:"recording_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.RecordingID == "" {
			return "Error: recording_id is required."
		}
		sq, err := h.chorusClient.CreateSalesQualification(args.RecordingID)
		if err != nil {
			return fmt.Sprintf("Error creating sales qualification: %v", err)
		}
		log.Printf("[user=%s channel=%s] created sales qualification for recording %s", userID, channelID, args.RecordingID)
		return chorus.FormatSalesQualification(sq)

	case ToolChorusGetSalesQualification:
		if errMsg := requireReady("Chorus", h.chorusClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			RecordingID string `json:"recording_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.RecordingID == "" {
			return "Error: recording_id is required."
		}
		sq, err := h.chorusClient.GetSalesQualification(args.RecordingID)
		if err != nil {
			return fmt.Sprintf("Error fetching sales qualification: %v", err)
		}
		log.Printf("[user=%s channel=%s] fetched sales qualification for recording %s", userID, channelID, args.RecordingID)
		return chorus.FormatSalesQualification(sq)

	case ToolChorusWritebackCRM:
		if errMsg := requireReady("Chorus", h.chorusClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			MeetingID     string `json:"meeting_id"`
			OpportunityID string `json:"opportunity_id"`
			ObjectType    string `json:"object_type"`
			CRMChanges    []struct {
				FieldName     string `json:"field_name"`
				NewValue      string `json:"new_value"`
				PreviousValue string `json:"previous_value"`
			} `json:"crm_changes"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.MeetingID == "" || args.OpportunityID == "" || len(args.CRMChanges) == 0 {
			return "Error: meeting_id, opportunity_id, and at least one crm_changes entry are required."
		}
		if args.ObjectType == "" {
			args.ObjectType = "Opportunity"
		}
		changes := make([]chorus.CRMChange, len(args.CRMChanges))
		for i, ch := range args.CRMChanges {
			changes[i] = chorus.CRMChange{
				FieldName:     ch.FieldName,
				NewValue:      ch.NewValue,
				PreviousValue: ch.PreviousValue,
			}
		}
		if err := h.chorusClient.WritebackCRM(args.MeetingID, args.ObjectType, args.OpportunityID, changes); err != nil {
			return fmt.Sprintf("Error writing back CRM fields: %v", err)
		}
		log.Printf("[user=%s channel=%s] wrote back %d CRM changes for meeting %s", userID, channelID, len(changes), args.MeetingID)
		return fmt.Sprintf("Successfully wrote %d CRM field update(s) for opportunity %s from meeting %s.", len(changes), args.OpportunityID, args.MeetingID)

	// ---- Datadog tools ----

	case ToolDatadogSearchLogs:
		if h.datadogClients == nil {
			return "Error: Datadog integration is not configured. Set DD_API_KEY_US/DD_APP_KEY_US and/or DD_API_KEY_EU/DD_APP_KEY_EU to enable it."
		}
		args, errMsg := parseToolArgs[struct {
			Query string `json:"query"`
			From  string `json:"from"`
			To    string `json:"to"`
			Limit int    `json:"limit"`
			Site  string `json:"site"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.Query == "" {
			return "Error: query is required."
		}
		result, err := h.datadogClients.SearchLogs(ctx, args.Site, args.Query, args.From, args.To, args.Limit)
		if err != nil {
			return fmt.Sprintf("Error searching Datadog logs: %v", err)
		}
		log.Printf("[user=%s channel=%s] searched Datadog logs for %q (site=%s)", userID, channelID, args.Query, args.Site)
		return result

	case ToolDatadogLogsAggregate:
		if h.datadogClients == nil {
			return "Error: Datadog integration is not configured. Set DD_API_KEY_US/DD_APP_KEY_US and/or DD_API_KEY_EU/DD_APP_KEY_EU to enable it."
		}
		args, errMsg := parseToolArgs[struct {
			Query   string      `json:"query"`
			GroupBy groupBySpec `json:"group_by"`
			Measure string      `json:"measure"`
			From    string      `json:"from"`
			To      string      `json:"to"`
			Site    string      `json:"site"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.Query == "" {
			return "Error: query is required."
		}
		result, data, err := h.datadogClients.AggregatePercentilesReport(ctx, args.Site, args.Query, args.From, args.To, args.GroupBy, args.Measure)
		if err != nil {
			return fmt.Sprintf("Error aggregating Datadog logs: %v", err)
		}
		log.Printf("[user=%s channel=%s] aggregated Datadog logs for %q (group_by=%v, measure=%s, site=%s)", userID, channelID, args.Query, []string(args.GroupBy), args.Measure, args.Site)
		// Cache the structured result (computed from the same single query) so
		// upload_aggregate_csv can attach the full dataset as a CSV file
		// without the model re-typing it inline.
		if len(data) > 0 {
			id := h.cacheAggregate(args.Query, data)
			result += fmt.Sprintf("\n\n(Full result cached as aggregate_id=%q. To attach the complete table(s) as a downloadable CSV file in Slack, call upload_aggregate_csv with this id — do NOT retype the rows.)", id)
		}
		return result

	case ToolDatadogListMonitors:
		if h.datadogClients == nil {
			return "Error: Datadog integration is not configured. Set DD_API_KEY_US/DD_APP_KEY_US and/or DD_API_KEY_EU/DD_APP_KEY_EU to enable it."
		}
		args, errMsg := parseToolArgs[struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
			Site  string `json:"site"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		result, err := h.datadogClients.ListMonitors(ctx, args.Site, args.Query, args.Limit)
		if err != nil {
			return fmt.Sprintf("Error listing Datadog monitors: %v", err)
		}
		log.Printf("[user=%s channel=%s] listed Datadog monitors (query=%q, site=%s)", userID, channelID, args.Query, args.Site)
		return result

	case ToolDatadogGetMonitor:
		if h.datadogClients == nil {
			return "Error: Datadog integration is not configured. Set DD_API_KEY_US/DD_APP_KEY_US and/or DD_API_KEY_EU/DD_APP_KEY_EU to enable it."
		}
		args, errMsg := parseToolArgs[struct {
			MonitorID string `json:"monitor_id"`
			Site      string `json:"site"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.MonitorID == "" {
			return "Error: monitor_id is required."
		}
		result, err := h.datadogClients.GetMonitor(ctx, args.Site, args.MonitorID)
		if err != nil {
			return fmt.Sprintf("Error fetching Datadog monitor: %v", err)
		}
		log.Printf("[user=%s channel=%s] fetched Datadog monitor %s (site=%s)", userID, channelID, args.MonitorID, args.Site)
		return result

	case ToolDatadogListHosts:
		if h.datadogClients == nil {
			return "Error: Datadog integration is not configured. Set DD_API_KEY_US/DD_APP_KEY_US and/or DD_API_KEY_EU/DD_APP_KEY_EU to enable it."
		}
		args, errMsg := parseToolArgs[struct {
			Filter string `json:"filter"`
			Count  int    `json:"count"`
			Site   string `json:"site"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		result, err := h.datadogClients.ListHosts(ctx, args.Site, args.Filter, args.Count)
		if err != nil {
			return fmt.Sprintf("Error listing Datadog hosts: %v", err)
		}
		log.Printf("[user=%s channel=%s] listed Datadog hosts (filter=%q, site=%s)", userID, channelID, args.Filter, args.Site)
		return result

	case ToolDatadogGetDashboard:
		if h.datadogClients == nil {
			return "Error: Datadog integration is not configured. Set DD_API_KEY_US/DD_APP_KEY_US and/or DD_API_KEY_EU/DD_APP_KEY_EU to enable it."
		}
		args, errMsg := parseToolArgs[struct {
			DashboardID string `json:"dashboard_id"`
			Site        string `json:"site"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.DashboardID == "" {
			return "Error: dashboard_id is required."
		}
		result, err := h.datadogClients.GetDashboard(ctx, args.Site, args.DashboardID)
		if err != nil {
			return fmt.Sprintf("Error fetching Datadog dashboard: %v", err)
		}
		log.Printf("[user=%s channel=%s] fetched Datadog dashboard %s (site=%s)", userID, channelID, args.DashboardID, args.Site)
		return result

	case ToolDatadogListDashboards:
		if h.datadogClients == nil {
			return "Error: Datadog integration is not configured. Set DD_API_KEY_US/DD_APP_KEY_US and/or DD_API_KEY_EU/DD_APP_KEY_EU to enable it."
		}
		args, errMsg := parseToolArgs[struct {
			Query string `json:"query"`
			Count int    `json:"count"`
			Site  string `json:"site"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		result, err := h.datadogClients.ListDashboards(ctx, args.Site, args.Query, args.Count)
		if err != nil {
			return fmt.Sprintf("Error listing Datadog dashboards: %v", err)
		}
		log.Printf("[user=%s channel=%s] listed Datadog dashboards (query=%q, site=%s)", userID, channelID, args.Query, args.Site)
		return result

	case ToolDatadogQueryMetrics:
		if h.datadogClients == nil {
			return "Error: Datadog integration is not configured. Set DD_API_KEY_US/DD_APP_KEY_US and/or DD_API_KEY_EU/DD_APP_KEY_EU to enable it."
		}
		args, errMsg := parseToolArgs[struct {
			Query string `json:"query"`
			From  string `json:"from"`
			To    string `json:"to"`
			Site  string `json:"site"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.Query) == "" {
			return "Error: query is required (e.g. 'avg:kubernetes.cpu.usage.total{*} by {kube_service}')."
		}
		result, err := h.datadogClients.QueryMetrics(ctx, args.Site, args.Query, args.From, args.To)
		if err != nil {
			return fmt.Sprintf("Error querying Datadog metrics: %v", err)
		}
		log.Printf("[user=%s channel=%s] queried Datadog metrics (site=%s, query=%q, from=%q, to=%q)", userID, channelID, args.Site, args.Query, args.From, args.To)
		return result

	// ---- AWS Cost Explorer tools ----

	case ToolAWSGetCostAndUsage:
		if h.awsClient == nil {
			return "Error: AWS integration is not configured. Set AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or AWS_PROFILE, or EKS IRSA via AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN) to enable Cost Explorer tools."
		}
		args, errMsg := parseToolArgs[struct {
			Start              string   `json:"start"`
			End                string   `json:"end"`
			Granularity        string   `json:"granularity"`
			Metric             string   `json:"metric"`
			GroupBy            string   `json:"group_by"`
			ServiceFilter      string   `json:"service_filter"`
			ExcludeChargeTypes []string `json:"exclude_charge_types"`
			ExcludeServices    []string `json:"exclude_services"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		res, err := h.awsClient.GetCostAndUsage(ctx, aws.CostAndUsageOpts{
			Start:              args.Start,
			End:                args.End,
			Granularity:        args.Granularity,
			Metric:             args.Metric,
			GroupBy:            args.GroupBy,
			ServiceFilter:      args.ServiceFilter,
			ExcludeChargeTypes: args.ExcludeChargeTypes,
			ExcludeServices:    args.ExcludeServices,
		})
		if err != nil {
			return fmt.Sprintf("Error fetching AWS cost and usage: %v", err)
		}
		log.Printf("[user=%s channel=%s] aws cost_and_usage (%s→%s, gran=%s, metric=%s, group_by=%s, filter=%q)",
			userID, channelID, res.Start, res.End, res.Granularity, res.Metric, res.GroupBy, args.ServiceFilter)
		return aws.FormatCostAndUsage(res)

	case ToolAWSGetCostForecast:
		if h.awsClient == nil {
			return "Error: AWS integration is not configured. Set AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or AWS_PROFILE, or EKS IRSA via AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN) to enable Cost Explorer tools."
		}
		args, errMsg := parseToolArgs[struct {
			Start              string   `json:"start"`
			End                string   `json:"end"`
			Granularity        string   `json:"granularity"`
			Metric             string   `json:"metric"`
			ExcludeChargeTypes []string `json:"exclude_charge_types"`
			ExcludeServices    []string `json:"exclude_services"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		res, err := h.awsClient.GetCostForecast(ctx, aws.ForecastOpts{
			Start:              args.Start,
			End:                args.End,
			Granularity:        args.Granularity,
			Metric:             args.Metric,
			ExcludeChargeTypes: args.ExcludeChargeTypes,
			ExcludeServices:    args.ExcludeServices,
		})
		if err != nil {
			return fmt.Sprintf("Error fetching AWS cost forecast: %v", err)
		}
		log.Printf("[user=%s channel=%s] aws cost_forecast (%s→%s, gran=%s, metric=%s)",
			userID, channelID, res.Start, res.End, res.Granularity, res.Metric)
		return aws.FormatForecast(res)

	case ToolAWSListDimensionValues:
		if h.awsClient == nil {
			return "Error: AWS integration is not configured. Set AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or AWS_PROFILE, or EKS IRSA via AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN) to enable Cost Explorer tools."
		}
		args, errMsg := parseToolArgs[struct {
			Dimension string `json:"dimension"`
			Start     string `json:"start"`
			End       string `json:"end"`
			Search    string `json:"search"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.Dimension == "" {
			return "Error: dimension is required (e.g. SERVICE, LINKED_ACCOUNT, REGION)."
		}
		res, err := h.awsClient.GetDimensionValues(ctx, aws.DimensionValuesOpts{
			Dimension: args.Dimension,
			Start:     args.Start,
			End:       args.End,
			Search:    args.Search,
		})
		if err != nil {
			return fmt.Sprintf("Error listing AWS dimension values: %v", err)
		}
		log.Printf("[user=%s channel=%s] aws list_dimension_values (%s, %s→%s, %d values)",
			userID, channelID, res.Dimension, res.Start, res.End, len(res.Values))
		return aws.FormatDimensionValues(res)

	case ToolAWSS3PutObject:
		if h.awsClient == nil {
			return "Error: AWS integration is not configured. Set AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or AWS_PROFILE, or EKS IRSA via AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN) to enable AWS S3 tools."
		}
		args, errMsg := parseToolArgs[struct {
			Bucket      string `json:"bucket"`
			Key         string `json:"key"`
			Content     string `json:"content"`
			ContentType string `json:"content_type"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		res, err := h.awsClient.S3PutObject(ctx, args.Bucket, args.Key, []byte(args.Content), args.ContentType)
		if err != nil {
			return fmt.Sprintf("Error writing S3 object: %v", err)
		}
		log.Printf("[user=%s channel=%s] aws s3_put_object (s3://%s/%s, region=%s, %d bytes)",
			userID, channelID, res.Bucket, res.Key, res.Region, res.Size)
		return aws.FormatS3Put(res)

	case ToolAWSS3GetObject:
		if h.awsClient == nil {
			return "Error: AWS integration is not configured. Set AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or AWS_PROFILE, or EKS IRSA via AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN) to enable AWS S3 tools."
		}
		args, errMsg := parseToolArgs[struct {
			Bucket string `json:"bucket"`
			Key    string `json:"key"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		res, err := h.awsClient.S3GetObject(ctx, args.Bucket, args.Key)
		if err != nil {
			return fmt.Sprintf("Error reading S3 object: %v", err)
		}
		log.Printf("[user=%s channel=%s] aws s3_get_object (s3://%s/%s, region=%s, %d bytes, truncated=%t)",
			userID, channelID, res.Bucket, res.Key, res.Region, res.Size, res.Truncated)
		return aws.FormatS3Get(res)

	case ToolAWSS3ListObjects:
		if h.awsClient == nil {
			return "Error: AWS integration is not configured. Set AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or AWS_PROFILE, or EKS IRSA via AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN) to enable AWS S3 tools."
		}
		args, errMsg := parseToolArgs[struct {
			Bucket  string `json:"bucket"`
			Prefix  string `json:"prefix"`
			MaxKeys int    `json:"max_keys"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		res, err := h.awsClient.S3ListObjects(ctx, args.Bucket, args.Prefix, args.MaxKeys)
		if err != nil {
			return fmt.Sprintf("Error listing S3 objects: %v", err)
		}
		log.Printf("[user=%s channel=%s] aws s3_list_objects (s3://%s/%s, region=%s, %d objects)",
			userID, channelID, res.Bucket, res.Prefix, res.Region, len(res.Objects))
		return aws.FormatS3List(res)

	// ---- Azure Cost Management tools ----

	case ToolAzureGetCostAndUsage:
		if h.azureClient == nil {
			return "Error: Azure integration is not configured. Set AZURE_TENANT_ID / AZURE_CLIENT_ID / AZURE_CLIENT_SECRET (and optionally AZURE_MANAGEMENT_GROUP_ID) to enable Azure Cost Management tools."
		}
		args, errMsg := parseToolArgs[struct {
			Start         string `json:"start"`
			End           string `json:"end"`
			Granularity   string `json:"granularity"`
			Metric        string `json:"metric"`
			GroupBy       string `json:"group_by"`
			ServiceFilter string `json:"service_filter"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		res, err := h.azureClient.GetCostAndUsage(ctx, azure.CostAndUsageOpts{
			Start:         args.Start,
			End:           args.End,
			Granularity:   args.Granularity,
			Metric:        args.Metric,
			GroupBy:       args.GroupBy,
			ServiceFilter: args.ServiceFilter,
		})
		if err != nil {
			return fmt.Sprintf("Error fetching Azure cost and usage: %v", err)
		}
		log.Printf("[user=%s channel=%s] azure cost_and_usage (%s→%s, gran=%s, metric=%s, group_by=%s, filter=%q)",
			userID, channelID, res.Start, res.End, res.Granularity, res.Metric, res.GroupBy, args.ServiceFilter)
		return azure.FormatCostAndUsage(res)

	case ToolAzureGetCostForecast:
		if h.azureClient == nil {
			return "Error: Azure integration is not configured. Set AZURE_TENANT_ID / AZURE_CLIENT_ID / AZURE_CLIENT_SECRET (and optionally AZURE_MANAGEMENT_GROUP_ID) to enable Azure Cost Management tools."
		}
		args, errMsg := parseToolArgs[struct {
			Start       string `json:"start"`
			End         string `json:"end"`
			Granularity string `json:"granularity"`
			Metric      string `json:"metric"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		res, err := h.azureClient.GetCostForecast(ctx, azure.ForecastOpts{
			Start:       args.Start,
			End:         args.End,
			Granularity: args.Granularity,
			Metric:      args.Metric,
		})
		if err != nil {
			return fmt.Sprintf("Error fetching Azure cost forecast: %v", err)
		}
		log.Printf("[user=%s channel=%s] azure cost_forecast (%s→%s, gran=%s, metric=%s)",
			userID, channelID, res.Start, res.End, res.Granularity, res.Metric)
		return azure.FormatForecast(res)

	case ToolAzureListDimensionValues:
		if h.azureClient == nil {
			return "Error: Azure integration is not configured. Set AZURE_TENANT_ID / AZURE_CLIENT_ID / AZURE_CLIENT_SECRET (and optionally AZURE_MANAGEMENT_GROUP_ID) to enable Azure Cost Management tools."
		}
		args, errMsg := parseToolArgs[struct {
			Dimension string `json:"dimension"`
			Search    string `json:"search"`
			Top       int    `json:"top"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.Dimension == "" {
			return "Error: dimension is required (e.g. ServiceName, ResourceGroupName, ResourceLocation)."
		}
		res, err := h.azureClient.GetDimensionValues(ctx, azure.DimensionValuesOpts{
			Dimension: args.Dimension,
			Search:    args.Search,
			Top:       args.Top,
		})
		if err != nil {
			return fmt.Sprintf("Error listing Azure dimension values: %v", err)
		}
		log.Printf("[user=%s channel=%s] azure list_dimension_values (%s, %d values)",
			userID, channelID, res.Dimension, len(res.Values))
		return azure.FormatDimensionValues(res)

	// ---- Databricks SQL (ovad only) ----

	case ToolDatabricksQuery:
		if errMsg := requireReady("Databricks", h.databricksClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			SQL        string `json:"sql"`
			Parameters []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
				Type  string `json:"type"`
			} `json:"parameters"`
			RowLimit int `json:"row_limit"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.SQL) == "" {
			return preconditionErrf("Error: sql is required (one or more read-only statements).")
		}
		params := make([]databricks.QueryParam, 0, len(args.Parameters))
		for _, p := range args.Parameters {
			if strings.TrimSpace(p.Name) == "" {
				return preconditionErrf("Error: each parameter needs a non-empty name matching a :marker in the SQL.")
			}
			params = append(params, databricks.QueryParam{Name: p.Name, Value: p.Value, Type: p.Type})
		}
		res, err := h.databricksClient.Query(ctx, args.SQL, params, args.RowLimit)
		if err != nil {
			return fmt.Sprintf("Error running Databricks query: %v", err)
		}
		log.Printf("[user=%s channel=%s] databricks_query (warehouse=%s, statement=%s, rows=%d, truncated=%t)",
			userID, channelID, res.WarehouseID, res.StatementID, res.RowCount, res.Truncated)
		return databricks.FormatQueryResult(res)

	// ---- ClickHouse Cloud billing (ovad only) ----

	case ToolClickHouseUsageCost:
		if errMsg := requireReady("ClickHouse", h.clickhouseClient); errMsg != "" {
			return errMsg
		}
		args, errMsg := parseToolArgs[struct {
			FromDate string   `json:"from_date"`
			ToDate   string   `json:"to_date"`
			Filters  []string `json:"filters"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.FromDate) == "" || strings.TrimSpace(args.ToDate) == "" {
			return preconditionErrf("Error: from_date and to_date are required (YYYY-MM-DD).")
		}
		res, err := h.clickhouseClient.GetUsageCost(ctx, args.FromDate, args.ToDate, args.Filters)
		if err != nil {
			return fmt.Sprintf("Error fetching ClickHouse usage cost: %v", err)
		}
		log.Printf("[user=%s channel=%s] clickhouse_usage_cost (org=%s, window=%s..%s, records=%d, grandTotalCHC=%.2f)",
			userID, channelID, res.OrganizationID, res.FromDate, res.ToDate, len(res.Records), res.GrandTotalCHC)
		return clickhouse.FormatUsageCost(res)

	case ToolClickHouseQuery:
		if h.clickhouseClient == nil || !h.clickhouseClient.QueryReady() {
			return preconditionErrf("Error: ClickHouse SQL query interface is not connected.")
		}
		args, errMsg := parseToolArgs[struct {
			SQL      string `json:"sql"`
			RowLimit int    `json:"row_limit"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.SQL) == "" {
			return preconditionErrf("Error: sql is required (a read-only ClickHouse statement).")
		}
		res, err := h.clickhouseClient.Query(ctx, args.SQL, args.RowLimit)
		if err != nil {
			return fmt.Sprintf("Error running ClickHouse query: %v", err)
		}
		log.Printf("[user=%s channel=%s] clickhouse_query (endpoint=%s, rows=%d, truncated=%t)",
			userID, channelID, res.Endpoint, res.RowCount, res.Truncated)
		return clickhouse.FormatSQLResult(res)

	case ToolFreshdeskListTickets:
		if h.freshworksClient == nil || !h.freshworksClient.Desk.Ready() {
			return preconditionErrf("Error: Freshdesk integration is not connected.")
		}
		args, errMsg := parseToolArgs[struct {
			UpdatedSince   string `json:"updated_since"`
			RequesterEmail string `json:"requester_email"`
			Page           int    `json:"page"`
			PerPage        int    `json:"per_page"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		tickets, err := h.freshworksClient.Desk.ListTickets(ctx, args.UpdatedSince, args.RequesterEmail, args.Page, args.PerPage)
		if err != nil {
			return fmt.Sprintf("Error listing Freshdesk tickets: %v", err)
		}
		log.Printf("[user=%s channel=%s] freshdesk_list_tickets (results=%d)", userID, channelID, len(tickets))
		return freshworks.FormatTicketList(tickets, "Freshdesk tickets")

	case ToolFreshdeskGetTicket:
		if h.freshworksClient == nil || !h.freshworksClient.Desk.Ready() {
			return preconditionErrf("Error: Freshdesk integration is not connected.")
		}
		args, errMsg := parseToolArgs[struct {
			TicketID             int64 `json:"ticket_id"`
			IncludeConversations *bool `json:"include_conversations"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.TicketID <= 0 {
			return preconditionErrf("Error: ticket_id is required.")
		}
		include := true
		if args.IncludeConversations != nil {
			include = *args.IncludeConversations
		}
		ticket, err := h.freshworksClient.Desk.GetTicket(ctx, args.TicketID, include)
		if err != nil {
			return fmt.Sprintf("Error fetching Freshdesk ticket: %v", err)
		}
		log.Printf("[user=%s channel=%s] freshdesk_get_ticket (id=%d)", userID, channelID, args.TicketID)
		return freshworks.FormatTicket(ticket)

	case ToolFreshdeskSearchTickets:
		if h.freshworksClient == nil || !h.freshworksClient.Desk.Ready() {
			return preconditionErrf("Error: Freshdesk integration is not connected.")
		}
		args, errMsg := parseToolArgs[struct {
			Query string `json:"query"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.Query) == "" {
			return preconditionErrf("Error: query is required.")
		}
		tickets, total, err := h.freshworksClient.Desk.SearchTickets(ctx, args.Query)
		if err != nil {
			return fmt.Sprintf("Error searching Freshdesk tickets: %v", err)
		}
		log.Printf("[user=%s channel=%s] freshdesk_search_tickets (query=%q, total=%d)", userID, channelID, args.Query, total)
		return freshworks.FormatTicketList(tickets, fmt.Sprintf("Freshdesk search (%d total)", total))

	case ToolFreshdeskFindAgent:
		if h.freshworksClient == nil || !h.freshworksClient.Desk.Ready() {
			return preconditionErrf("Error: Freshdesk integration is not connected.")
		}
		args, errMsg := parseToolArgs[struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.Email) == "" && strings.TrimSpace(args.Name) == "" {
			return preconditionErrf("Error: email or name is required.")
		}
		agents, err := h.freshworksClient.Desk.FindAgents(ctx, args.Email, args.Name)
		if err != nil {
			return fmt.Sprintf("Error finding Freshdesk agent: %v", err)
		}
		log.Printf("[user=%s channel=%s] freshdesk_find_agent (matches=%d)", userID, channelID, len(agents))
		return freshworks.FormatAgents(agents)

	case ToolFreshdeskListTicketFields:
		if h.freshworksClient == nil || !h.freshworksClient.Desk.Ready() {
			return preconditionErrf("Error: Freshdesk integration is not connected.")
		}
		fields, err := h.freshworksClient.Desk.ListTicketFields(ctx)
		if err != nil {
			return fmt.Sprintf("Error listing Freshdesk ticket fields: %v", err)
		}
		log.Printf("[user=%s channel=%s] freshdesk_list_ticket_fields (count=%d)", userID, channelID, len(fields))
		return freshworks.FormatTicketFields(fields)

	case ToolFreshchatGetConversation:
		if h.freshworksClient == nil || !h.freshworksClient.Chat.Ready() {
			return preconditionErrf("Error: Freshchat integration is not connected.")
		}
		args, errMsg := parseToolArgs[struct {
			ConversationID string `json:"conversation_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.ConversationID) == "" {
			return preconditionErrf("Error: conversation_id is required.")
		}
		conv, err := h.freshworksClient.Chat.GetConversation(ctx, args.ConversationID)
		if err != nil {
			return fmt.Sprintf("Error fetching Freshchat conversation: %v", err)
		}
		log.Printf("[user=%s channel=%s] freshchat_get_conversation (id=%s)", userID, channelID, args.ConversationID)
		return freshworks.FormatChatConversation(conv)

	case ToolFreshchatGetConversationMessages:
		if h.freshworksClient == nil || !h.freshworksClient.Chat.Ready() {
			return preconditionErrf("Error: Freshchat integration is not connected.")
		}
		args, errMsg := parseToolArgs[struct {
			ConversationID string `json:"conversation_id"`
			Page           int    `json:"page"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.ConversationID) == "" {
			return preconditionErrf("Error: conversation_id is required.")
		}
		msgs, err := h.freshworksClient.Chat.GetConversationMessages(ctx, args.ConversationID, args.Page)
		if err != nil {
			return fmt.Sprintf("Error fetching Freshchat messages: %v", err)
		}
		log.Printf("[user=%s channel=%s] freshchat_get_conversation_messages (id=%s, messages=%d)", userID, channelID, args.ConversationID, len(msgs))
		return freshworks.FormatChatMessages(args.ConversationID, msgs)

	case ToolFreshworksCRMSearch:
		if h.freshworksClient == nil || !h.freshworksClient.CRM.Ready() {
			return preconditionErrf("Error: Freshworks CRM integration is not connected.")
		}
		args, errMsg := parseToolArgs[struct {
			Query    string   `json:"query"`
			Entities []string `json:"entities"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if strings.TrimSpace(args.Query) == "" {
			return preconditionErrf("Error: query is required.")
		}
		results, err := h.freshworksClient.CRM.Search(ctx, args.Query, args.Entities)
		if err != nil {
			return fmt.Sprintf("Error searching Freshworks CRM: %v", err)
		}
		log.Printf("[user=%s channel=%s] freshworks_crm_search (query=%q, results=%d)", userID, channelID, args.Query, len(results))
		return freshworks.FormatCRMSearch(args.Query, results)

	case ToolFreshworksCRMGetContact:
		if h.freshworksClient == nil || !h.freshworksClient.CRM.Ready() {
			return preconditionErrf("Error: Freshworks CRM integration is not connected.")
		}
		args, errMsg := parseToolArgs[struct {
			ContactID int64 `json:"contact_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.ContactID <= 0 {
			return preconditionErrf("Error: contact_id is required.")
		}
		contact, err := h.freshworksClient.CRM.GetContact(ctx, args.ContactID)
		if err != nil {
			return fmt.Sprintf("Error fetching Freshworks CRM contact: %v", err)
		}
		log.Printf("[user=%s channel=%s] freshworks_crm_get_contact (id=%d)", userID, channelID, args.ContactID)
		return freshworks.FormatCRMContact(contact)

	case ToolFreshworksCRMGetDeal:
		if h.freshworksClient == nil || !h.freshworksClient.CRM.Ready() {
			return preconditionErrf("Error: Freshworks CRM integration is not connected.")
		}
		args, errMsg := parseToolArgs[struct {
			DealID int64 `json:"deal_id"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		if args.DealID <= 0 {
			return preconditionErrf("Error: deal_id is required.")
		}
		deal, err := h.freshworksClient.CRM.GetDeal(ctx, args.DealID)
		if err != nil {
			return fmt.Sprintf("Error fetching Freshworks CRM deal: %v", err)
		}
		log.Printf("[user=%s channel=%s] freshworks_crm_get_deal (id=%d)", userID, channelID, args.DealID)
		return freshworks.FormatCRMDeal(deal)

	case ToolHTTPGet:
		args, errMsg := parseToolArgs[struct {
			URL      string `json:"url"`
			Accept   string `json:"accept"`
			MaxBytes int    `json:"max_bytes"`
		}](argsJSON)
		if errMsg != "" {
			return errMsg
		}
		return doHTTPGet(ctx, args.URL, args.Accept, args.MaxBytes, userID, channelID)

	default:
		return fmt.Sprintf("Unknown tool: %s", name)
	}
}

func (h *GeneralHandler) fetchWorkflowLogs(ctx context.Context, text, userID, channelID string) string {
	return fetchWorkflowLogsBulk(ctx, h.ghClient, text, userID, channelID)
}

func (h *GeneralHandler) replyDefault(channelID, responseURL, auditTS, text string) {
	replyOrThread(h.slackClient, channelID, responseURL, auditTS, text)
}

// isCodeIntent returns true when the user's message suggests code modification,
// code review, file reading, or any GitHub interaction — tasks that benefit from the specialised CODE_MODEL.
//
// Intentionally strict: bare nouns like "secret", "config", "permission",
// "deploy" are NOT code signals on their own — they appear frequently in ops
// / troubleshooting questions ("permission denied fetching secret X") where
// the lighter general model is more appropriate and the user usually just
// needs a clarifying question, not a multi-round repo expedition. Require
// an explicit code action verb, a GitHub noun (repo / branch / PR / commit),
// or a recognised programming-language file extension.
func isCodeIntent(text string) bool {
	codeKeywords := []string{
		// Code modification — explicit action verbs.
		"modify", "change the code", "change code", "edit the file", "edit file",
		"update the file", "update file", "fix the code", "fix code", "fix the bug",
		"create pr", "create a pr", "open pr", "open a pr", "pull request",
		"refactor", "implement", "add feature", "write code", "patch",
		"change all", "update all", "set all", "replace all",
		"change tag", "change version", "tag version",
		"send me pr", "send pr",
		// Code review & reading — require an explicit code reference.
		"check the code", "check code", "read the code", "read code",
		"show me the code", "show the code", "show code", "code review",
		"analyze the code", "analyze code", "analyse the code", "analyse code",
		"explain the code", "explain code",
		"search code", "search the code", "find in code",
		// Security code-audit signals (kept narrow).
		"vulnerability", "cve",
		// Org-wide / cross-repo search.
		"every repo", "all repo", "across repo", "across the org",
		"which service", "which repo", "which project",
		"find usages", "find usage",
		// GitHub interactions — unambiguous code/repo nouns only.
		"repo", "repository", "branch", "commit", "merge",
		"github", "workflow", "pipeline",
		// Dashboards & workflows — structured JSON + tool-composition tasks
		// benefit from the sharper CODE_MODEL reasoning.
		"dashboard", "dashboards",
	}
	for _, kw := range codeKeywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	// Check for known programming-language file extensions in the text
	// (sourced from github-linguist — 1400+ extensions).
	for _, match := range extensionRe.FindAllString(text, -1) {
		if knownExtensions[strings.ToLower(match)] {
			return true
		}
	}
	return false
}

var preActionAckRe = regexp.MustCompile(`(?i)\b(on it|i(?:'|’)m\s+(checking|looking|working|updating|investigating)|i\s+will|i(?:'|’)ll|we\s+will|will\s+return|let me check|let me take a look)\b`)

func isPreActionAck(text string) bool {
	return preActionAckRe.MatchString(strings.TrimSpace(text))
}
