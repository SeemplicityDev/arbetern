package commands

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/journal"
	"github.com/justmike1/arbetern/internal/progress"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/llm"
	"github.com/justmike1/arbetern/metrics"
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
	"modify_file": true, "create_file": true, "delete_file": true, "move_file": true, "regex_replace_file": true,
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
	"delete_file":        true,
	"move_file":          true,
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

// loopResult is how the loop ended, with the turn's cumulative usage and the
// timing split between the model and the tools it called.
type loopResult struct {
	Outcome loopOutcome
	Final   string
	// Degraded marks a Final the loop produced by giving up rather than by
	// answering. It is still replied to the requester, but it is never
	// recorded as a remembered turn: a give-up reply keeps the question it
	// failed on, so stored it would win the semantic match against the real
	// answer to the same question later.
	Degraded      bool
	FinishReason  string
	LastTruncated string
	Model         string
	Rounds        int
	ToolCalls     int
	Usage         llm.Usage
	Compression   llm.CompressionStats
	FirstResponse time.Duration
	ModelTime     time.Duration
	ToolTime      time.Duration
}

// outcomeNames map how the loop ended onto the stable labels the performance
// series is aggregated by.
var outcomeNames = map[loopOutcome]string{
	loopCompleted:      metrics.OutcomeCompleted,
	loopNoChoices:      metrics.OutcomeNoChoices,
	loopTruncatedCalls: metrics.OutcomeTruncated,
	loopPreActionAck:   metrics.OutcomeBlocked,
	loopEmpty:          metrics.OutcomeEmpty,
	loopMaxRounds:      metrics.OutcomeMaxRounds,
}

// recordTurn hands the finished turn's timing to the performance series. The
// sample carries the agent, entry path, model and outcome — never the
// requester, the channel or the prompt.
func (h *GeneralHandler) recordTurn(res loopResult, started time.Time, err error) {
	if h.perf == nil {
		return
	}
	outcome := metrics.OutcomeError
	if err == nil {
		if name, ok := outcomeNames[res.Outcome]; ok {
			outcome = name
		}
	}
	src := h.source()
	h.perf.RecordTurn(metrics.Turn{
		Agent:           h.agentID,
		Source:          src,
		Model:           res.Model,
		Outcome:         outcome,
		DurationMS:      time.Since(started).Milliseconds(),
		FirstResponseMS: res.FirstResponse.Milliseconds(),
		ModelMS:         res.ModelTime.Milliseconds(),
		ToolMS:          res.ToolTime.Milliseconds(),
		Rounds:          res.Rounds,
		ToolCalls:       res.ToolCalls,
		OutputTokens:    res.Usage.CompletionTokens,
		Failed:          outcome != metrics.OutcomeCompleted,
	})
}

func (h *GeneralHandler) recordTool(name string, took time.Duration, result string) {
	if h.perf == nil {
		return
	}
	h.perf.RecordTool(metrics.ToolRun{
		Agent:     h.agentID,
		Name:      name,
		LatencyMS: took.Milliseconds(),
		Failed:    toolFailed(result),
	})
}

// toolFailed reports whether a tool result is the error convention every tool
// in this package follows: a plain string starting with "Error".
func toolFailed(result string) bool { return strings.HasPrefix(result, "Error") }

// runToolLoop drives the model until it answers, gives up, or runs out of
// rounds, and records the turn's usage on the way out.
func (h *GeneralHandler) runToolLoop(ctx context.Context, lp toolLoop) (res loopResult, err error) {
	active := lp.client
	messages := lp.messages
	rounds := lp.rounds
	if rounds <= 0 {
		rounds = defaultToolRounds
	}
	started := time.Now()
	defer func() {
		res.Model = active.Model()
		h.recordUsage(res.Model, lp.userID, res.Usage, res.Compression)
		h.recordTurn(res, started, err)
	}()

	var blockedAcks, emptyRetries, truncatedRounds, narrationRetries int
	for i := 0; i < rounds; i++ {
		res.Rounds = i + 1
		roundStart := time.Now()
		resp, err := active.CompleteWithTools(ctx, messages, lp.tools)
		took := time.Since(roundStart)
		res.ModelTime += took
		if res.FirstResponse == 0 {
			res.FirstResponse = took
		}
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
			if name := narratedToolCall(content, lp.tools); name != "" {
				if narrationRetries < maxNarrationRetries {
					narrationRetries++
					log.Printf("%s model described a %s call instead of making it (retry %d)", lp.logPrefix, name, narrationRetries)
					messages = append(messages, llm.NewChatMessage("assistant", content), llm.NewChatMessage("user", narrationNudge(name)))
					continue
				}
				log.Printf("%s still narrating a %s call after %d retries; replying without remembering the turn", lp.logPrefix, name, narrationRetries)
				res.Degraded = true
			}
			res.Outcome = loopCompleted
			res.Final = content
			return res, nil
		}

		// Keep any text the model wrote alongside its tool calls. Dropping it
		// loses the turn's stated plan from both the transcript the model sees
		// on later rounds and the progress the user sees while it works.
		messages = append(messages, llm.ChatMessage{Role: "assistant", Content: choice.Message.Content, ToolCalls: choice.Message.ToolCalls})
		lp.progress.SetPlan(choice.Message.Content)
		res.ToolCalls += len(toolCalls)
		for _, tc := range toolCalls {
			lp.progress.ToolCalled(tc.Function.Name)
		}
		// The model may ask for several independent tools in one round. Running
		// them together makes the round cost the slowest call rather than their
		// sum. Results are collected by index so the order the model sees never
		// depends on which call finished first.
		toolStart := time.Now()
		results := h.executeToolsConcurrently(ctx, lp, toolCalls)
		res.ToolTime += time.Since(toolStart)
		for i, tc := range toolCalls {
			name := tc.Function.Name
			if lp.afterTool != nil {
				lp.afterTool(name, results[i])
			}
			messages = append(messages, llm.NewToolResultMessage(tc.ID, stripPreconditionPrefix(results[i])))
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

// maxConcurrentTools bounds how many tools from one round run at once, so a
// round that asks for many calls cannot open an unbounded number of API
// connections at the same time.
const maxConcurrentTools = 4

// executeToolsConcurrently runs a round's tool calls and returns their results
// in call order. A single call runs inline so the common case adds no
// scheduling overhead. Mutating tools run one at a time in their given order:
// two writes to the same branch or ticket must not interleave.
func (h *GeneralHandler) executeToolsConcurrently(ctx context.Context, lp toolLoop, calls []llm.ToolCall) []string {
	results := make([]string, len(calls))
	run := func(i int) {
		tc := calls[i]
		log.Printf("%s tool: %s(%s)", lp.logPrefix, tc.Function.Name, redactToolArgsForLog(tc.Function.Name, tc.Function.Arguments))
		toolStart := time.Now()
		results[i] = h.executeTool(ctx, lp.channelID, lp.userID, lp.auditTS, tc.Function.Name, tc.Function.Arguments)
		h.recordTool(tc.Function.Name, time.Since(toolStart), results[i])
	}
	mutating := anyMutating(calls)
	if mutating {
		// Recorded before the call, not after: a crash between the write and
		// the record is exactly the case this guards against, and an entry
		// that says "no changes yet" would then be replayed on top of a pull
		// request that already exists.
		journal.NoteSideEffect(ctx)
	}
	if len(calls) == 1 || mutating {
		for i := range calls {
			run(i)
		}
		return results
	}
	sem := make(chan struct{}, maxConcurrentTools)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			safego.Run("tool: "+calls[i].Function.Name, func() { run(i) })
		}(i)
	}
	wg.Wait()
	for i := range results {
		if results[i] == "" {
			results[i] = "Error: the tool did not return a result."
		}
	}
	return results
}

func anyMutating(calls []llm.ToolCall) bool {
	for _, tc := range calls {
		if mutatingTools[tc.Function.Name] {
			return true
		}
	}
	return false
}

func previewText(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
