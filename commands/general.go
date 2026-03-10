package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/chorus"
	"github.com/justmike1/arbetern/config"
	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/llm"
	"github.com/justmike1/arbetern/nvd"
	"github.com/justmike1/arbetern/salesforce"
	"github.com/justmike1/arbetern/slack"
)

// knownExtensions is the set of all known programming-language file extensions
// sourced from github-linguist/linguist. Built once at init time.
var knownExtensions map[string]bool

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
	contextProvider  *ContextProvider
	memory           *ConversationMemory
	prompts          PromptProvider
	agentID          string
	appURL           string
	maxToolRounds    int
	userContext      string
	currentChannelID string
	currentAuditTS   string
	// activeBranches tracks branches created during this Execute() run.
	// Key: "owner/repo", Value: branch metadata. This ensures multiple
	// modify_file calls for the same repo produce a single PR.
	activeBranches map[string]*activeBranchInfo
}

type activeBranchInfo struct {
	branchName string
	baseBranch string
	prURL      string
}

func (h *GeneralHandler) Execute(channelID, userID, text, responseURL, auditTS string) {
	ctx := context.Background()
	h.currentChannelID = channelID
	h.currentAuditTS = auditTS
	h.activeBranches = make(map[string]*activeBranchInfo)

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

	// Track cumulative token usage across all LLM rounds.
	var totalUsage llm.Usage

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
			totalUsage.CompletionTokens += resp.Usage.CompletionTokens
			totalUsage.TotalTokens += resp.Usage.TotalTokens
		}

		if len(resp.Choices) == 0 {
			log.Printf("[user=%s channel=%s] LLM returned no choices", userID, channelID)
			h.replyDefault(channelID, responseURL, auditTS, "No response from the model.")
			return
		}

		choice := resp.Choices[0]

		if len(choice.Message.ToolCalls) == 0 {
			log.Printf("[user=%s channel=%s] general query completed successfully", userID, channelID)
			h.memory.SetAssistantResponse(channelID, userID, choice.Message.Content)
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
			result := h.executeTool(ctx, channelID, userID, auditTS, tc.Function.Name, tc.Function.Arguments)
			messages = append(messages, llm.NewToolResultMessage(tc.ID, result))
			if tc.Function.Name == "reply_in_thread" && !strings.HasPrefix(result, "Error") {
				repliedInThread = true
			}
			// Dynamically switch to the code model once code-related
			// tools are invoked (covers cases where initial intent detection
			// didn't trigger the code model).
			codeTools := map[string]bool{
				"modify_file": true, "regex_replace_file": true,
				"get_file_content": true,
				"search_code":      true, "search_code_org": true, "search_files": true,
				"list_directory": true, "get_pull_request": true,
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

func (h *GeneralHandler) systemPrompt() string {
	return h.prompts.SystemPrompt("general")
}

func (h *GeneralHandler) buildTools() []llm.Tool {
	tools := []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "list_org_repos",
				Description: "List all repositories in the GitHub organization that the bot has access to. Use ONLY when the user explicitly asks to list/discover repos, or when no repository was provided and repo discovery is required.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "list_user_repos",
				Description: "List all repositories accessible by the authenticated GitHub user. Use ONLY when the user explicitly asks to list/discover repos, or when no repository was provided and repo discovery is required.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_file_content",
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
				Name:        "get_repo_default_branch",
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
				Name:        "get_authenticated_user",
				Description: "Get the GitHub username of the authenticated bot user.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "resolve_owner",
				Description: "Resolve the GitHub organization or user that owns repositories.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "fetch_channel_context",
				Description: "Fetch recent messages from the current Slack channel for additional context about the ongoing conversation.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "search_files",
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
				Name:        "list_directory",
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
				Name:        "modify_file",
				Description: "Modify a file in a GitHub repository using a safe find-and-replace approach. Provide the exact text to find (old_content) and the replacement text (new_content). The tool reads the FULL file from GitHub, performs the replacement, then creates a branch, commits, and opens a PR. Multiple modify_file calls for the SAME repository are automatically grouped into a SINGLE pull request — so when implementing a change that touches several files, just call modify_file for each file and all changes will land in one PR. IMPORTANT: old_content must be an exact substring of the current file — include enough surrounding lines (3-5) will ensure a unique match. Only the matched section is replaced; the rest of the file is preserved.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"path":{"type":"string","description":"File path within the repository"},
						"old_content":{"type":"string","description":"The exact text in the current file to find and replace. Include 3-5 surrounding context lines to ensure a unique match."},
						"new_content":{"type":"string","description":"The replacement text that will replace old_content."},
						"description":{"type":"string","description":"Short description of what was changed (used as commit message and PR title)"},
						"branch":{"type":"string","description":"Base branch name (optional, uses default branch if empty)"}
					},
					"required":["repo","path","old_content","new_content","description"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "regex_replace_file",
				Description: "Bulk-replace all matches of a SIMPLE regex pattern in a file. Best for uniform single-line replacements across a whole file (e.g. 'change all image.tag to latest'). Do NOT use for scoped/structural changes in a specific section — use modify_file instead for those. Keep patterns short and per-line. Avoid complex multi-line regex with (?:.|\\n)*? or lookaheads — if you need those, use modify_file. The tool reads the FULL file from GitHub, applies a Go RE2 regex replacement on ALL matches, creates a branch, commits, and opens a PR. Multiple calls for the same repo are grouped into a SINGLE PR. Replacement supports $1, $2 for captured groups.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"path":{"type":"string","description":"File path within the repository"},
						"pattern":{"type":"string","description":"Go (RE2) regular expression to match. Use capturing groups for partial replacements (e.g. '(tag:\\s*)\\S+' to capture the 'tag: ' prefix)."},
						"replacement":{"type":"string","description":"Replacement string. Use $1, $2, etc. to reference captured groups (e.g. '${1}latest')."},
						"description":{"type":"string","description":"Short description of what was changed (used as commit message and PR title)"},
						"branch":{"type":"string","description":"Base branch name (optional, uses default branch if empty)"}
					},
					"required":["repo","path","pattern","replacement","description"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_pull_request",
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
				Name:        "list_pull_requests",
				Description: "List recent pull requests in a repository. Useful for finding relevant PRs by title, discovering recent changes, or identifying the PR that introduced a particular change.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"repo":{"type":"string","description":"Repository name (without owner)"},
						"state":{"type":"string","description":"Filter by state: 'open', 'closed', or 'all' (default: 'all')"},
						"limit":{"type":"integer","description":"Maximum number of PRs to return (default: 10, max: 30)"}
					},
					"required":["repo"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "search_code",
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
				Name:        "search_code_org",
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
				Name:        "get_workflow_run",
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
				Name:        "rerun_failed_jobs",
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
				Name:        "rerun_workflow",
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
				Name:        "reply_in_thread",
				Description: "Post a message as a threaded reply to a specific Slack message. Use this when the user asks you to reply inside someone's thread or respond to a particular message. You need the thread_ts of the target message from the channel context. IMPORTANT: Messages marked [BOT] are this bot's own messages — never reply to those. Always use the thread_ts of the HUMAN user's message (e.g. the person mentioned by name like 'Shahar', 'John', etc.).",
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
				Name:        "upload_snippet",
				Description: "Upload a text file snippet to Slack. Use this instead of posting a long message when the content is large (e.g., full search results with file paths and code lines, log dumps, CSV data, large code blocks). The snippet appears as a collapsible file attachment in the channel/thread, keeping the conversation clean. PREFER this over reply_in_thread or plain messages whenever the output would exceed ~30 lines or ~2000 characters. Ideal for: org-wide search results, full file listings, detailed tables, log output, code dumps.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"content":{"type":"string","description":"The text content of the snippet."},
						"filename":{"type":"string","description":"Filename for the snippet (e.g. 'search-results.txt', 'clickhouse-usages.csv', 'report.md'). Use an appropriate extension."},
						"title":{"type":"string","description":"Display title for the snippet in Slack (e.g. 'clickhouse_connection usages across org')."},
						"filetype":{"type":"string","description":"Slack file type for syntax highlighting (e.g. 'text', 'markdown', 'csv', 'python', 'yaml', 'json'). Default: 'text'."}
					},
					"required":["content","filename","title"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "fetch_thread_context",
				Description: "Fetch the full conversation from a Slack thread URL. Use this FIRST whenever the user provides a Slack thread/message link (https://...slack.com/archives/...) to read the thread's content before acting on it (e.g., creating a Jira ticket, summarizing, replying). Returns all messages in the thread. The response also includes the channel_id and thread_ts so you can reply_in_thread afterwards.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"url":{"type":"string","description":"Slack thread or message URL (e.g. 'https://yourorg.slack.com/archives/C01BS13KFL7/p1771847194296799')"}
					},
					"required":["url"]
				}`),
			},
		},
	}

	// NVD CVE lookup tools are always available (NVD client is always created).
	if h.nvdClient != nil {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "lookup_cve",
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
				Name:        "search_cve",
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

	// Salesforce tools are only available when Salesforce is connected.
	if h.sfClient != nil && h.sfClient.Ready() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "salesforce_query",
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
				Name:        "salesforce_describe",
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

	// Chorus tools are only available when Chorus is connected.
	if h.chorusClient != nil && h.chorusClient.Ready() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "chorus_list_conversations",
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
				Name:        "chorus_get_conversation",
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
				Name:        "chorus_create_sales_qualification",
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
				Name:        "chorus_get_sales_qualification",
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
				Name:        "chorus_writeback_crm",
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

	// Jira tools are only available when Jira is connected.
	if h.jiraClient != nil && h.jiraClient.Ready() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "create_jira_ticket",
				Description: "Create a Jira ticket (issue). Use this when the user asks to create a ticket, task, story, or bug from the conversation content (e.g., a test plan, action item, or bug report). Populate the summary and description from the relevant content discussed in the conversation. IMPORTANT: Format the description using markdown — use # for headers, - for bullet lists, 1) for numbered lists, **bold** for emphasis, and `code` for inline code. Structure the ticket professionally with clear sections (e.g., ## Context, ## Scope, ## Acceptance Criteria). If the user asks to assign the ticket to a person, use the assignee field. If the user asks to assign to a team, use the team field. Both can be used at the same time.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"summary":{"type":"string","description":"Short one-line title for the ticket."},
						"description":{"type":"string","description":"Detailed, well-structured description using markdown formatting. Use ## for section headers, - for bullet points, 1) for numbered steps, **bold** for key terms, and backticks for code references. Organize into clear sections like Context, Scope, Test Plan, Acceptance Criteria, References, etc."},
						"issue_type":{"type":"string","description":"Issue type: 'Task', 'Bug', 'Story', 'Epic', etc. Default: 'Task'."},
						"labels":{"type":"array","items":{"type":"string"},"description":"Optional labels to apply to the ticket (e.g. ['qa','automated-test'])."},
						"assignee":{"type":"string","description":"Name of the person to assign the ticket to (e.g. 'Udi', 'John Smith'). The system will search for a matching Jira user."},
						"team":{"type":"string","description":"Name of the team to assign the ticket to (e.g. 'Application', 'DevOps', 'asgard'). The system will search for a matching Jira team."}
					},
					"required":["summary","description"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "list_jira_projects",
				Description: "List all Jira projects visible to the bot. Use this to discover available project keys before creating a ticket.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "search_jira_issues",
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
				Name:        "get_jira_issue",
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
				Name:        "update_jira_issue",
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
		})
	}

	// Slack user info tool is always available.
	tools = append(tools, llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "get_slack_user_info",
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

	// Jira user resolution tool — resolves a person's name/email to their Jira account ID.
	if h.jiraClient != nil && h.jiraClient.Ready() {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "resolve_jira_user",
				Description: "Search for a Jira user by name and/or email and return their account ID. IMPORTANT: Jira Cloud JQL does NOT reliably support searching by display name (e.g. assignee = 'Mike Joseph' may return zero results). You MUST call this tool first to get the user's Jira account ID, then use that account ID in JQL queries (e.g. assignee = 'accountId'). This is the ONLY reliable way to find issues by assignee in Jira Cloud. ALWAYS pass both name AND email (from get_slack_user_info) for best results — email-based search is the most reliable.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"name":{"type":"string","description":"The person's display name (e.g. 'Mike Joseph', 'John Smith')"},
						"email":{"type":"string","description":"The person's email address (most reliable for Jira lookup). Get this from get_slack_user_info."}
					},
					"required":["name"]
				}`),
			},
		}, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "resolve_jira_team",
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
				Name:        "get_jira_dashboard",
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
				Name:        "get_jira_filter",
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
				Name:        "search_confluence_pages",
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
				Name:        "get_confluence_page",
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
				Name:        "list_confluence_spaces",
				Description: "List all Confluence spaces visible to the bot. Useful for discovering available spaces before searching for pages. Returns space keys, names, and types.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		})
	}

	return tools
}

func (h *GeneralHandler) executeTool(ctx context.Context, channelID, userID, auditTS, name, argsJSON string) string {
	switch name {
	case "list_org_repos":
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

	case "list_user_repos":
		repos, err := h.ghClient.ListUserRepos(ctx)
		if err != nil {
			return fmt.Sprintf("Error listing user repos: %v", err)
		}
		if len(repos) == 0 {
			return "No repositories found for the authenticated user."
		}
		log.Printf("[user=%s channel=%s] listed %d user repos", userID, channelID, len(repos))
		return fmt.Sprintf("Repositories (%d):\n%s", len(repos), strings.Join(repos, "\n"))

	case "get_file_content":
		var args struct {
			Repo   string `json:"repo"`
			Path   string `json:"path"`
			Branch string `json:"branch"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		branch := args.Branch
		if branch == "" {
			branch, err = h.ghClient.GetDefaultBranch(ctx, owner, args.Repo)
			if err != nil {
				return fmt.Sprintf("Error getting default branch: %v", err)
			}
		}
		content, _, err := h.ghClient.GetFileContent(ctx, owner, args.Repo, args.Path, branch)
		if err != nil {
			hint := ""
			if strings.Contains(err.Error(), "404") {
				hint = " This path may be a directory, or it may be nested under a provider subdirectory (e.g. aws/, azure/). Try list_directory on the parent path to discover the correct structure, then read the files you need."
			}
			return fmt.Sprintf("Error reading file: %v.%s", err, hint)
		}
		if len(content) > 8000 {
			content = content[:8000] + "\n... (truncated — file is longer than shown, important content may follow)"
		}
		return content

	case "get_repo_default_branch":
		var args struct {
			Repo string `json:"repo"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "get_authenticated_user":
		user, err := h.ghClient.GetAuthenticatedUser(ctx)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return fmt.Sprintf("Authenticated as: %s", user)

	case "resolve_owner":
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return fmt.Sprintf("Resolved owner: %s", owner)

	case "search_files":
		var args struct {
			Repo    string `json:"repo"`
			Pattern string `json:"pattern"`
			Branch  string `json:"branch"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		branch := args.Branch
		if branch == "" {
			branch, err = h.ghClient.GetDefaultBranch(ctx, owner, args.Repo)
			if err != nil {
				return fmt.Sprintf("Error getting default branch: %v", err)
			}
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

	case "list_directory":
		var args struct {
			Repo   string `json:"repo"`
			Path   string `json:"path"`
			Branch string `json:"branch"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		branch := args.Branch
		if branch == "" {
			branch, err = h.ghClient.GetDefaultBranch(ctx, owner, args.Repo)
			if err != nil {
				return fmt.Sprintf("Error getting default branch: %v", err)
			}
		}
		entries, err := h.ghClient.GetDirectoryContents(ctx, owner, args.Repo, args.Path, branch)
		if err != nil {
			return fmt.Sprintf("Error listing directory: %v", err)
		}
		log.Printf("[user=%s channel=%s] listed directory %s/%s/%s (%d entries)", userID, channelID, args.Repo, branch, args.Path, len(entries))
		return fmt.Sprintf("Contents of %s/%s:\n%s", args.Repo, args.Path, strings.Join(entries, "\n"))

	case "fetch_channel_context":
		context, err := h.contextProvider.GetChannelContext(channelID)
		if err != nil {
			return fmt.Sprintf("Error fetching channel context: %v", err)
		}
		log.Printf("[user=%s channel=%s] fetched channel context via tool", userID, channelID)
		return context

	case "modify_file":
		var args struct {
			Repo        string `json:"repo"`
			Path        string `json:"path"`
			OldContent  string `json:"old_content"`
			NewContent  string `json:"new_content"`
			Description string `json:"description"`
			Branch      string `json:"branch"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		baseBranch := args.Branch
		if baseBranch == "" {
			baseBranch, err = h.ghClient.GetDefaultBranch(ctx, owner, args.Repo)
			if err != nil {
				return fmt.Sprintf("Error getting default branch: %v", err)
			}
		}

		// Reuse an existing branch for this repo if one was created earlier in this session.
		repoKey := owner + "/" + args.Repo
		active := h.activeBranches[repoKey]

		// Determine which branch to read the file from.
		// If we already have an active branch, read from it (it may contain prior commits).
		readBranch := baseBranch
		if active != nil {
			readBranch = active.branchName
		}

		fullContent, fileSHA, err := h.ghClient.GetFileContent(ctx, owner, args.Repo, args.Path, readBranch)
		if err != nil {
			return fmt.Sprintf("Error reading current file: %v", err)
		}
		// Perform find-and-replace on the full file content.
		if !strings.Contains(fullContent, args.OldContent) {
			return "Error: old_content not found in the file. Make sure old_content is an exact substring of the current file (including whitespace and indentation). Re-read the file with get_file_content and try again."
		}
		occurrences := strings.Count(fullContent, args.OldContent)
		if occurrences > 1 {
			return fmt.Sprintf("Error: old_content matches %d locations in the file. Include more surrounding context lines to make it unique.", occurrences)
		}
		updatedContent := strings.Replace(fullContent, args.OldContent, args.NewContent, 1)

		if active == nil {
			// First modification for this repo — create a new branch and PR.
			branchName := github.GenerateBranchName(h.agentID)
			if err := h.ghClient.CreateBranch(ctx, owner, args.Repo, baseBranch, branchName); err != nil {
				return fmt.Sprintf("Error creating branch: %v", err)
			}
			commitMsg := fmt.Sprintf("%s: %s", h.agentID, args.Description)
			if err := h.ghClient.UpdateFile(ctx, owner, args.Repo, args.Path, branchName, commitMsg, []byte(updatedContent), fileSHA); err != nil {
				return fmt.Sprintf("Error committing file: %v", err)
			}
			prTitle := fmt.Sprintf("%s: %s", h.agentID, args.Description)
			prBody := fmt.Sprintf("Automated change requested via Slack by <@%s>.\n\nChange: %s", userID, args.Description)
			prURL, err := h.ghClient.CreatePullRequest(ctx, owner, args.Repo, baseBranch, branchName, prTitle, prBody)
			if err != nil {
				return fmt.Sprintf("Changes committed to branch %s but PR creation failed: %v", branchName, err)
			}
			h.activeBranches[repoKey] = &activeBranchInfo{
				branchName: branchName,
				baseBranch: baseBranch,
				prURL:      prURL,
			}
			log.Printf("[user=%s channel=%s] PR created via modify_file: %s", userID, channelID, prURL)
			return fmt.Sprintf("Pull request created: %s", prURL)
		}

		// Subsequent modification — commit to the existing branch.
		commitMsg := fmt.Sprintf("%s: %s", h.agentID, args.Description)
		if err := h.ghClient.UpdateFile(ctx, owner, args.Repo, args.Path, active.branchName, commitMsg, []byte(updatedContent), fileSHA); err != nil {
			return fmt.Sprintf("Error committing file to existing branch: %v", err)
		}
		log.Printf("[user=%s channel=%s] additional commit to branch %s for PR: %s", userID, channelID, active.branchName, active.prURL)
		return fmt.Sprintf("Changes committed to existing PR: %s", active.prURL)

	case "regex_replace_file":
		var args struct {
			Repo        string `json:"repo"`
			Path        string `json:"path"`
			Pattern     string `json:"pattern"`
			Replacement string `json:"replacement"`
			Description string `json:"description"`
			Branch      string `json:"branch"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
		}
		re, err := regexp.Compile(args.Pattern)
		if err != nil {
			return fmt.Sprintf("Error compiling regex pattern: %v", err)
		}
		owner, err := h.ghClient.ResolveOwner(ctx)
		if err != nil {
			return fmt.Sprintf("Error resolving owner: %v", err)
		}
		baseBranch := args.Branch
		if baseBranch == "" {
			baseBranch, err = h.ghClient.GetDefaultBranch(ctx, owner, args.Repo)
			if err != nil {
				return fmt.Sprintf("Error getting default branch: %v", err)
			}
		}

		repoKey := owner + "/" + args.Repo
		active := h.activeBranches[repoKey]

		readBranch := baseBranch
		if active != nil {
			readBranch = active.branchName
		}

		fullContent, fileSHA, err := h.ghClient.GetFileContent(ctx, owner, args.Repo, args.Path, readBranch)
		if err != nil {
			return fmt.Sprintf("Error reading current file: %v", err)
		}

		updatedContent := re.ReplaceAllString(fullContent, args.Replacement)
		if updatedContent == fullContent {
			return "Error: regex pattern matched nothing in the file. Double-check the pattern and try again."
		}
		matches := len(re.FindAllStringIndex(fullContent, -1))
		log.Printf("[user=%s channel=%s] regex_replace_file: %d matches of %q in %s/%s",
			userID, channelID, matches, args.Pattern, args.Repo, args.Path)

		if active == nil {
			branchName := github.GenerateBranchName(h.agentID)
			if err := h.ghClient.CreateBranch(ctx, owner, args.Repo, baseBranch, branchName); err != nil {
				return fmt.Sprintf("Error creating branch: %v", err)
			}
			commitMsg := fmt.Sprintf("%s: %s", h.agentID, args.Description)
			if err := h.ghClient.UpdateFile(ctx, owner, args.Repo, args.Path, branchName, commitMsg, []byte(updatedContent), fileSHA); err != nil {
				return fmt.Sprintf("Error committing file: %v", err)
			}
			prTitle := fmt.Sprintf("%s: %s", h.agentID, args.Description)
			prBody := fmt.Sprintf("Automated regex replacement requested via Slack by <@%s>.\n\nChange: %s\nPattern: `%s` → `%s`\nMatches replaced: %d", userID, args.Description, args.Pattern, args.Replacement, matches)
			prURL, err := h.ghClient.CreatePullRequest(ctx, owner, args.Repo, baseBranch, branchName, prTitle, prBody)
			if err != nil {
				return fmt.Sprintf("Changes committed to branch %s but PR creation failed: %v", branchName, err)
			}
			h.activeBranches[repoKey] = &activeBranchInfo{
				branchName: branchName,
				baseBranch: baseBranch,
				prURL:      prURL,
			}
			log.Printf("[user=%s channel=%s] PR created via regex_replace_file (%d matches): %s", userID, channelID, matches, prURL)
			return fmt.Sprintf("Replaced %d matches. Pull request created: %s", matches, prURL)
		}

		commitMsg := fmt.Sprintf("%s: %s", h.agentID, args.Description)
		if err := h.ghClient.UpdateFile(ctx, owner, args.Repo, args.Path, active.branchName, commitMsg, []byte(updatedContent), fileSHA); err != nil {
			return fmt.Sprintf("Error committing file to existing branch: %v", err)
		}
		log.Printf("[user=%s channel=%s] regex_replace_file: additional commit to branch %s (%d matches) for PR: %s", userID, channelID, active.branchName, matches, active.prURL)
		return fmt.Sprintf("Replaced %d matches. Changes committed to existing PR: %s", matches, active.prURL)

	case "get_pull_request":
		var args struct {
			Repo   string `json:"repo"`
			Number int    `json:"number"`
			URL    string `json:"url"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "list_pull_requests":
		var args struct {
			Repo  string `json:"repo"`
			State string `json:"state"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "search_code":
		var args struct {
			Repo  string `json:"repo"`
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "search_code_org":
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "get_workflow_run":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "rerun_failed_jobs":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "rerun_workflow":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "reply_in_thread":
		var args struct {
			ThreadTS string `json:"thread_ts"`
			Text     string `json:"text"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
		}
		if err := h.slackClient.PostThreadReply(channelID, args.ThreadTS, args.Text); err != nil {
			return fmt.Sprintf("Error posting thread reply: %v", err)
		}
		log.Printf("[user=%s channel=%s] posted thread reply to ts=%s", userID, channelID, args.ThreadTS)
		return "Successfully posted reply in thread."

	case "upload_snippet":
		var args struct {
			Content  string `json:"content"`
			Filename string `json:"filename"`
			Title    string `json:"title"`
			Filetype string `json:"filetype"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
		}
		if args.Filetype == "" {
			args.Filetype = "text"
		}
		// Post snippet in the current thread if we have one, otherwise in the channel.
		threadTS := ""
		if h.currentAuditTS != "" {
			threadTS = h.currentAuditTS
		}
		fileID, err := h.slackClient.UploadFileSnippet(channelID, threadTS, args.Filename, args.Title, args.Content, args.Filetype)
		if err != nil {
			return fmt.Sprintf("Error uploading snippet: %v", err)
		}
		log.Printf("[user=%s channel=%s] uploaded file snippet %s (%s)", userID, channelID, fileID, args.Filename)
		return fmt.Sprintf("Successfully uploaded snippet '%s' as %s.", args.Title, args.Filename)

	case "fetch_thread_context":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "create_jira_ticket":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Jira integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			Project     string   `json:"project"`
			Summary     string   `json:"summary"`
			Description string   `json:"description"`
			IssueType   string   `json:"issue_type"`
			Labels      []string `json:"labels"`
			Assignee    string   `json:"assignee"`
			Team        string   `json:"team"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

		// Resolve assignee name to Jira account ID.
		var assigneeID string
		if args.Assignee != "" {
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

		issue, err := h.jiraClient.CreateIssue(atlassian.CreateIssueInput{
			Project:     args.Project,
			Summary:     args.Summary,
			Description: args.Description,
			IssueType:   args.IssueType,
			Labels:      args.Labels,
			AssigneeID:  assigneeID,
		})
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

		log.Printf("[user=%s channel=%s] created Jira ticket %s: %s", userID, channelID, issue.Key, issue.Browse)
		return fmt.Sprintf("Jira ticket created: *%s* — %s\nSummary: %s", issue.Key, issue.Browse, args.Summary)

	case "list_jira_projects":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Jira integration is not connected. It may still be initializing — please try again shortly."
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

	case "search_jira_issues":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Jira integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			JQL        string `json:"jql"`
			MaxResults int    `json:"max_results"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "get_jira_issue":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Jira integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			IssueKey string `json:"issue_key"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "update_jira_issue":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Jira integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			IssueKey    string `json:"issue_key"`
			Summary     string `json:"summary"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "get_slack_user_info":
		var args struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
		}
		user, err := h.slackClient.GetUserInfo(args.UserID)
		if err != nil {
			return fmt.Sprintf("Error getting user info: %v", err)
		}
		return fmt.Sprintf("Slack User Info:\n  User ID: %s\n  Real Name: %s\n  Display Name: %s\n  Email: %s\n  Title: %s",
			user.ID, user.RealName, user.Profile.DisplayName, user.Profile.Email, user.Profile.Title)

	case "resolve_jira_team":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Jira integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			TeamName string `json:"team_name"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "resolve_jira_user":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Jira integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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
			// /user/search often matches prefixes, and "Mike Joseph" as a single
			// query may fail while "Mike" succeeds.
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

	case "get_jira_dashboard":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Jira integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			DashboardID string `json:"dashboard_id"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "get_jira_filter":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Jira integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			FilterID   string `json:"filter_id"`
			RunJQL     *bool  `json:"run_jql"`
			MaxResults int    `json:"max_results"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "search_confluence_pages":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Atlassian integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			CQL   string `json:"cql"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "get_confluence_page":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Atlassian integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			PageID string `json:"page_id"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "list_confluence_spaces":
		if h.jiraClient == nil || !h.jiraClient.Ready() {
			return "Error: Atlassian integration is not connected. It may still be initializing — please try again shortly."
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

	case "lookup_cve":
		if h.nvdClient == nil {
			return "Error: NVD integration is not configured."
		}
		var args struct {
			CVEID string `json:"cve_id"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "search_cve":
		if h.nvdClient == nil {
			return "Error: NVD integration is not configured."
		}
		var args struct {
			Keyword        string `json:"keyword"`
			ResultsPerPage int    `json:"results_per_page"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "salesforce_query":
		if h.sfClient == nil || !h.sfClient.Ready() {
			return "Error: Salesforce integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			SOQL string `json:"soql"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "salesforce_describe":
		if h.sfClient == nil || !h.sfClient.Ready() {
			return "Error: Salesforce integration is not connected. It may still be initializing — please try again shortly."
		}
		var args struct {
			ObjectName string `json:"object_name"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "chorus_list_conversations":
		if h.chorusClient == nil || !h.chorusClient.Ready() {
			return "Error: Chorus integration is not connected. Set CHORUS_API_TOKEN to enable it."
		}
		var args struct {
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
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "chorus_get_conversation":
		if h.chorusClient == nil || !h.chorusClient.Ready() {
			return "Error: Chorus integration is not connected. Set CHORUS_API_TOKEN to enable it."
		}
		var args struct {
			ConversationID string `json:"conversation_id"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "chorus_create_sales_qualification":
		if h.chorusClient == nil || !h.chorusClient.Ready() {
			return "Error: Chorus integration is not connected. Set CHORUS_API_TOKEN to enable it."
		}
		var args struct {
			RecordingID string `json:"recording_id"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "chorus_get_sales_qualification":
		if h.chorusClient == nil || !h.chorusClient.Ready() {
			return "Error: Chorus integration is not connected. Set CHORUS_API_TOKEN to enable it."
		}
		var args struct {
			RecordingID string `json:"recording_id"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	case "chorus_writeback_crm":
		if h.chorusClient == nil || !h.chorusClient.Ready() {
			return "Error: Chorus integration is not connected. Set CHORUS_API_TOKEN to enable it."
		}
		var args struct {
			MeetingID     string `json:"meeting_id"`
			OpportunityID string `json:"opportunity_id"`
			ObjectType    string `json:"object_type"`
			CRMChanges    []struct {
				FieldName     string `json:"field_name"`
				NewValue      string `json:"new_value"`
				PreviousValue string `json:"previous_value"`
			} `json:"crm_changes"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
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

	default:
		return fmt.Sprintf("Unknown tool: %s", name)
	}
}

func (h *GeneralHandler) fetchWorkflowLogs(ctx context.Context, text, userID, channelID string) string {
	urls := github.ExtractWorkflowRunURLs(text)
	if len(urls) == 0 {
		return ""
	}

	seen := make(map[string]bool)
	var result string
	for _, u := range urls {
		if seen[u] {
			continue
		}
		seen[u] = true

		owner, repo, runID, err := github.ParseWorkflowRunURL(u)
		if err != nil {
			continue
		}

		log.Printf("[user=%s channel=%s] auto-fetching workflow run %s/%s/%d", userID, channelID, owner, repo, runID)
		summary, err := h.ghClient.GetWorkflowRunSummary(ctx, owner, repo, runID)
		if err != nil {
			log.Printf("[user=%s channel=%s] failed to fetch workflow run summary: %v", userID, channelID, err)
			continue
		}

		result += github.FormatWorkflowRunSummary(summary)
	}
	return result
}

func (h *GeneralHandler) replyDefault(channelID, responseURL, auditTS, text string) {
	if auditTS != "" {
		if err := h.slackClient.PostThreadReply(channelID, auditTS, text); err != nil {
			log.Printf("[channel=%s] failed to post thread reply: %v", channelID, err)
		}
		return
	}
	if err := slack.RespondToURL(responseURL, text, false); err != nil {
		log.Printf("[channel=%s] failed to respond: %v", channelID, err)
	}
}

// isCodeIntent returns true when the user's message suggests code modification,
// code review, file reading, or any GitHub interaction — tasks that benefit from the specialised CODE_MODEL.
func isCodeIntent(text string) bool {
	codeKeywords := []string{
		// Code modification
		"modify", "change the code", "change code", "edit the file", "edit file",
		"update the file", "update file", "fix the code", "fix code", "fix the bug",
		"create pr", "create a pr", "open pr", "open a pr", "pull request",
		"refactor", "implement", "add feature", "write code", "patch",
		// File changes (broad — catches "change all X to Y in file")
		"change all", "update all", "set all", "replace all",
		"change tag", "change version", "tag version",
		"send me pr", "send pr",
		// Code review & reading
		"review", "look at", "check the code", "check code", "read the code", "read code",
		"show me the code", "show the code", "show code", "code review",
		"analyze the code", "analyze code", "analyse the code", "analyse code",
		"inspect", "examine", "audit", "vulnerability", "cve",
		"affected", "exposed", "security", "dependency", "dependencies",
		"what does", "how does", "explain the code", "explain code",
		"search code", "search the code", "find in code", "look for",
		// Org-wide / cross-repo search
		"every repo", "all repo", "across repo", "across the org",
		"which service", "which repo", "which project",
		"find usages", "find usage", "find all", "give me a list",
		"who uses", "what uses", "check every",
		// GitHub interactions — always use code model for any repo/GitHub work
		"repo", "repository", "branch", "commit", "merge",
		"github", "workflow", "pipeline", "ci/cd", "cicd",
		"actions", "deploy", "deployment",
		"file", "directory", "folder", "path",
		"secret", "config", "configuration",
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
