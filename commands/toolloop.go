package commands

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/justmike1/arbetern/internal/progress"
	"github.com/justmike1/arbetern/llm"
)

type loopOutcome int

const (
	loopCompleted loopOutcome = iota
	loopNoChoices
	loopTruncatedCalls
	loopPreActionAck
	loopEmpty
	loopMaxRounds
)

const (
	defaultToolRounds   = 50
	maxNarrationRetries = 2
	maxPreActionAcks    = 3

	preActionAckNudge  = "Do not send pre-action acknowledgements. Start tool execution now. Return only completed results from tool outputs (or a concrete tool error after attempted execution)."
	truncatedTurnNudge = "Your previous response hit the output-length limit before it produced anything. Take ONE smaller step now: make a single tool call (e.g. modify one file), or give a brief answer. Keep the response concise so it fits within the limit."
	emptyTurnNudge     = "Your previous response was empty. Please provide a complete answer to my original request. Use the available tools if needed."

	truncatedCallsFallback = "I stopped here: my tool calls kept getting cut off at the output limit, and I won't run a half-written change (that is how a PR ends up with no title or description). Try narrowing the request — one file or one change at a time."
	emptyTurnFallback      = "I couldn't complete this request — it looks like it was too large to finish in one pass (my response kept hitting the output limit). Try breaking it into smaller steps and I'll pick it up from there."
)

// codeModelTools are the tools whose first use switches the turn to the code model.
var codeModelTools = map[string]bool{
	"modify_file": true, "create_file": true, "regex_replace_file": true,
	"get_file_content": true,
	"search_code":      true, "search_code_org": true, "search_files": true,
	"list_directory": true, "get_pull_request": true,
	"create_dashboard": true, "create_workflow": true, "update_workflow": true, "call_workflow": true,
}

// mutatingTools produce side effects; a failed call means the turn did not
// achieve its purpose even when the model reports success.
var mutatingTools = map[string]bool{
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

// toolLoop configures one run of the agent's tool-use loop.
type toolLoop struct {
	logPrefix         string
	client            *llm.Client
	codeClient        *llm.Client
	tools             []llm.Tool
	messages          []llm.ChatMessage
	rounds            int
	emptyRetries      int
	guardPreActionAck bool
	channelID         string
	userID            string
	auditTS           string
	progress          *progress.Tracker
	afterTool         func(name, result string)
}

// loopResult is how the loop ended, with the turn's cumulative usage.
type loopResult struct {
	Outcome       loopOutcome
	Final         string
	FinishReason  string
	LastTruncated string
	Model         string
	Rounds        int
	ToolCalls     int
	Usage         llm.Usage
	Compression   llm.CompressionStats
}

// runToolLoop drives the model until it answers, gives up, or runs out of
// rounds, and records the turn's usage on the way out.
func (h *GeneralHandler) runToolLoop(ctx context.Context, lp toolLoop) (res loopResult, err error) {
	active := lp.client
	messages := lp.messages
	rounds := lp.rounds
	if rounds <= 0 {
		rounds = defaultToolRounds
	}
	defer func() {
		res.Model = active.Model()
		h.recordUsage(res.Model, lp.userID, res.Usage, res.Compression)
	}()

	var blockedAcks, emptyRetries, truncatedRounds, narrationRetries int
	for i := 0; i < rounds; i++ {
		res.Rounds = i + 1
		resp, err := active.CompleteWithTools(ctx, messages, lp.tools)
		if err != nil {
			return res, err
		}
		if resp.Usage != nil {
			res.Usage.Add(*resp.Usage)
		}
		if resp.Compression != nil {
			res.Compression.Add(*resp.Compression)
		}
		if resp.Error != nil {
			return res, fmt.Errorf("%s", resp.Error.Message)
		}
		if len(resp.Choices) == 0 {
			log.Printf("%s LLM returned no choices", lp.logPrefix)
			res.Outcome = loopNoChoices
			return res, nil
		}
		choice := resp.Choices[0]
		res.FinishReason = choice.FinishReason

		toolCalls, truncated := splitTruncatedToolCalls(choice.FinishReason, choice.Message.ToolCalls)
		if truncated != nil {
			truncatedRounds++
			log.Printf("%s discarded truncated %s call (finish=%q, round %d/%d); kept %d complete call(s)",
				lp.logPrefix, truncated.Function.Name, choice.FinishReason, truncatedRounds, maxTruncatedToolRounds, len(toolCalls))
			if truncatedRounds > maxTruncatedToolRounds {
				res.Outcome = loopTruncatedCalls
				res.LastTruncated = truncated.Function.Name
				return res, nil
			}
		}

		if len(toolCalls) == 0 && truncated == nil {
			content := choice.Message.Content
			if lp.guardPreActionAck && res.ToolCalls == 0 && isPreActionAck(content) {
				blockedAcks++
				log.Printf("%s blocked pre-action ack reply (%d); forcing tool execution retry", lp.logPrefix, blockedAcks)
				if blockedAcks >= maxPreActionAcks {
					res.Outcome = loopPreActionAck
					return res, nil
				}
				messages = append(messages, llm.NewChatMessage("assistant", content), llm.NewChatMessage("user", preActionAckNudge))
				continue
			}
			content = strings.TrimSpace(content)
			if content == "" {
				if emptyRetries < lp.emptyRetries {
					emptyRetries++
					nudge := emptyTurnNudge
					if isTruncatedFinish(choice.FinishReason) {
						log.Printf("%s response truncated at output limit (finish=%q, retry %d); asking for a smaller step", lp.logPrefix, choice.FinishReason, emptyRetries)
						nudge = truncatedTurnNudge
					} else {
						log.Printf("%s model returned empty content (retry %d); nudging for a real response", lp.logPrefix, emptyRetries)
					}
					messages = append(messages, llm.NewChatMessage("assistant", emptyTurnPlaceholder), llm.NewChatMessage("user", nudge))
					continue
				}
				log.Printf("%s ended with empty content after %d retries (finish=%q)", lp.logPrefix, emptyRetries, choice.FinishReason)
				res.Outcome = loopEmpty
				return res, nil
			}
			if name := narratedToolCall(content, lp.tools); name != "" && narrationRetries < maxNarrationRetries {
				narrationRetries++
				log.Printf("%s model described a %s call instead of making it (retry %d)", lp.logPrefix, name, narrationRetries)
				messages = append(messages, llm.NewChatMessage("assistant", content), llm.NewChatMessage("user", narrationNudge(name)))
				continue
			}
			res.Outcome = loopCompleted
			res.Final = content
			return res, nil
		}

		messages = append(messages, llm.ChatMessage{Role: "assistant", ToolCalls: choice.Message.ToolCalls})
		for _, tc := range toolCalls {
			name := tc.Function.Name
			log.Printf("%s tool: %s(%s)", lp.logPrefix, name, redactToolArgsForLog(name, tc.Function.Arguments))
			res.ToolCalls++
			lp.progress.ToolCalled(name)
			result := h.executeTool(ctx, lp.channelID, lp.userID, lp.auditTS, name, tc.Function.Arguments)
			if lp.afterTool != nil {
				lp.afterTool(name, result)
			}
			messages = append(messages, llm.NewToolResultMessage(tc.ID, stripPreconditionPrefix(result)))
			if codeModelTools[name] && lp.codeClient != nil && active != lp.codeClient {
				active = lp.codeClient
				log.Printf("%s switched to code model (%s) after %s call", lp.logPrefix, active.Model(), name)
			}
		}
		if truncated != nil {
			messages = append(messages, llm.NewToolResultMessage(truncated.ID, truncatedToolCallResult))
		}
	}
	log.Printf("%s exceeded max tool rounds (%d)", lp.logPrefix, rounds)
	res.Outcome = loopMaxRounds
	return res, nil
}

func previewText(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
