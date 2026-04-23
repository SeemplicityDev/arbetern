package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/justmike1/arbetern/llm"
	"github.com/justmike1/arbetern/workflows"
)

// workflowTools returns the LLM tools for creating, listing, deleting, and
// chaining scheduled workflows. The create / delete tools are suppressed when
// the handler is running inside a workflow tick (h.headless) so a workflow
// cannot recursively spawn more workflows. The `call_workflow` and
// `list_workflows` tools stay available inside headless ticks since they are
// the canonical way to express the "flow of deployments" pattern.
func (h *GeneralHandler) workflowTools() []llm.Tool {
	if h.workflows == nil {
		return nil
	}
	tools := []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "call_workflow",
				Description: "Run another workflow once, synchronously, by id or short_name. Use this to compose workflows: a parent workflow can farm sub-tasks out to more specialised child workflows (the 'flow of deployments' pattern). Returns the child workflow's final result text so the caller can chain reasoning on top of it. Does not affect the child's own schedule.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"agent":{"type":"string","description":"Agent id that owns the target workflow (e.g. 'pulse', 'ovad'). Defaults to the current agent."},
						"id":{"type":"string","description":"Workflow id (16-hex from the view URL) OR the short_name. Use list_workflows to discover."}
					},
					"required":["id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "list_workflows",
				Description: "List the scheduled workflows currently owned by this agent, returning their ids, names, short names, intervals, patterns, and last-run timestamps.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
	}
	if h.headless {
		return tools
	}
	return append(tools,
		llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "create_workflow",
				Description: "Create a scheduled or event-triggered workflow owned by the current agent. Each run re-invokes this same agent's tool-loop so prompts can freely use Jira search, GitHub PR creation, Slack posting (post_slack_message), etc. Four execution patterns are supported:\n\n  - Monoflow: supply `prompt` only. One LLM call per tick.\n  - Flow of subflows: supply `tasks` (ordered list of {name, prompt}). Each task runs sequentially; outputs thread forward as context.\n  - Flow of deployments: write a `prompt` that instructs the agent to use `call_workflow` against already-registered child workflows.\n  - Event-triggered: set `trigger.type` to 'on_success' or 'on_failure' and `trigger.ref` to '<agent>/<id>' of the upstream workflow. Interval is ignored.\n\nUse scheduled monoflow when the user says 'every N minutes', 'poll X and do Y'. Use tasks for multi-step recurring processes. Use event triggers when something should happen 'after workflow X succeeds / fails'. A JSON descriptor is persisted at <WORKFLOWS_DIR>/<agent>/<id>.json and an HTML viewer is rendered at /" + h.agentID + "/workflow/<id>. Returns the id, short_name, and view URL which you MUST include in your reply.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"name":{"type":"string","description":"Human title of the workflow."},
						"short_name":{"type":"string","description":"Short slug shown on the agent card (lowercase, hyphenated, <=24 chars)."},
						"description":{"type":"string","description":"One-sentence summary."},
						"interval":{"type":"string","description":"Go duration between ticks for scheduled workflows (1m-168h, default 5m). Ignored for event-triggered or manual workflows."},
						"run_at_utc":{"type":"string","description":"Optional HH:MM UTC time-of-day for the FIRST scheduled tick. The workflow then re-runs every 'interval'. Use this for daily/weekly digests that must fire at a specific wall-clock time (e.g. '05:00' for a 5 AM UTC daily report). Omit for 'run immediately on start, then every interval'."},
						"prompt":{"type":"string","description":"Monoflow prompt. Required unless 'tasks' is provided. Must be complete and self-contained (include channel IDs, project keys, repos, labels, assignees)."},
						"tasks":{"type":"array","description":"Ordered multi-step task list (flow-of-subflows pattern). Each task's output is fed into the next task's context.","items":{"type":"object","properties":{"name":{"type":"string"},"prompt":{"type":"string"}},"required":["name","prompt"]}},
						"trigger":{"type":"object","description":"Execution trigger. Omit for schedule. Use {type:'on_success'|'on_failure', ref:'<agent>/<id>'} for event-driven. Use {type:'manual'} to disable ticks entirely.","properties":{"type":{"type":"string","enum":["schedule","on_success","on_failure","manual"]},"ref":{"type":"string"}}}
					},
					"required":["name","short_name"]
				}`),
			},
		},
		llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "delete_workflow",
				Description: "Stop a workflow's background execution, remove its stored JSON, and make its view URL return 404. Requires the workflow id.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"id":{"type":"string","description":"Workflow id (the 16-hex segment from the view URL)."}
					},
					"required":["id"]
				}`),
			},
		},
		llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "update_workflow",
				Description: "Edit an existing workflow owned by the current agent. Only the fields you pass are changed; all others (including run history, id, created_at, and short_name) are preserved. Use this when the user asks to 'update', 'edit', 'change', 'tweak', 'amend', 'fix', or 'modify' a workflow's behaviour — for example changing the prompt to always post a Slack message, adjusting the interval, pausing/resuming, switching trigger type, or rewriting the task list. The tick goroutine is restarted so the change takes effect on the next run. Requires the workflow id (discover via list_workflows if the user only gave a name). Returns the updated descriptor and its view URL.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"id":{"type":"string","description":"Workflow id (16-hex segment from the view URL). Required."},
						"name":{"type":"string","description":"New human title. Omit to leave unchanged."},
						"description":{"type":"string","description":"New one-sentence summary. Omit to leave unchanged."},
						"interval":{"type":"string","description":"New Go duration between ticks (1m-168h). Omit to leave unchanged."},
						"run_at_utc":{"type":"string","description":"New HH:MM UTC time-of-day for the first scheduled tick. Pass an empty string to clear (run immediately on start, then every interval). Omit to leave unchanged."},
						"prompt":{"type":"string","description":"New monoflow prompt. Omit to leave unchanged. Pass an empty string ONLY if you are simultaneously providing a non-empty 'tasks' array."},
						"tasks":{"type":"array","description":"Replacement ordered task list. Omit to leave unchanged. Pass an empty array to switch the workflow to a prompt-only monoflow.","items":{"type":"object","properties":{"name":{"type":"string"},"prompt":{"type":"string"}},"required":["name","prompt"]}},
						"trigger":{"type":"object","description":"Replacement trigger. Omit to leave unchanged.","properties":{"type":{"type":"string","enum":["schedule","on_success","on_failure","manual"]},"ref":{"type":"string"}}},
						"enabled":{"type":"boolean","description":"Pause (false) or resume (true) the workflow. Omit to leave unchanged."}
					},
					"required":["id"]
				}`),
			},
		},
	)
}

// executeWorkflowTool dispatches the workflow-related LLM tool calls.
// Returns ("", false) when the tool name is not a workflow tool.
func (h *GeneralHandler) executeWorkflowTool(ctx context.Context, userID, channelID, name, argsJSON string) (string, bool) {
	if h.workflows == nil {
		return "", false
	}
	switch name {
	case "create_workflow":
		if h.headless {
			return "Error: workflows cannot create other workflows from inside a scheduled tick.", true
		}
		var args struct {
			Name        string            `json:"name"`
			ShortName   string            `json:"short_name"`
			Description string            `json:"description"`
			Interval    string            `json:"interval"`
			RunAtUTC    string            `json:"run_at_utc"`
			Prompt      string            `json:"prompt"`
			Tasks       []workflows.Task  `json:"tasks"`
			Trigger     workflows.Trigger `json:"trigger"`
		}
		if msg := unmarshalArgs(argsJSON, &args); msg != "" {
			return msg, true
		}
		if args.Name == "" {
			return "Error: 'name' is required.", true
		}
		if strings.TrimSpace(args.Prompt) == "" && len(args.Tasks) == 0 {
			return "Error: either 'prompt' or a non-empty 'tasks' list is required.", true
		}
		interval := args.Interval
		if interval == "" {
			interval = workflows.DefaultInterval.String()
		} else if _, err := time.ParseDuration(interval); err != nil {
			return fmt.Sprintf("Error: invalid interval %q: %v", interval, err), true
		}
		w, err := h.workflows.Create(ctx, workflows.CreateOpts{
			Agent:       h.agentID,
			CreatedBy:   userID,
			Name:        args.Name,
			ShortName:   args.ShortName,
			Description: args.Description,
			Interval:    interval,
			RunAtUTC:    args.RunAtUTC,
			Prompt:      args.Prompt,
			Tasks:       args.Tasks,
			Trigger:     args.Trigger,
		})
		if err != nil {
			return fmt.Sprintf("Error creating workflow: %v", err), true
		}
		log.Printf("[user=%s channel=%s] created workflow agent=%s id=%s pattern=%s", userID, channelID, h.agentID, w.ID, w.Pattern())
		url := h.appURL + w.ViewURL()
		return fmt.Sprintf("Created %s workflow %q (id=%s, short=%s). View: %s", w.Pattern(), w.Name, w.ID, w.ShortName, url), true

	case "list_workflows":
		list := h.workflows.List(h.agentID)
		if len(list) == 0 {
			return "No workflows are currently registered for this agent.", true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Workflows for %s (%d):\n", h.agentID, len(list))
		for _, w := range list {
			lastRun := w.LastRun
			if lastRun == "" {
				lastRun = "pending"
			}
			fmt.Fprintf(&b, "- %s — %s (short=%s, pattern=%s, every %s, last run %s)\n  %s\n", w.ID, w.Name, w.ShortName, w.Pattern(), w.Interval, lastRun, h.appURL+w.ViewURL())
		}
		return b.String(), true

	case "delete_workflow":
		if h.headless {
			return "Error: workflows cannot delete workflows from inside a scheduled tick.", true
		}
		var args struct {
			ID string `json:"id"`
		}
		if msg := unmarshalArgs(argsJSON, &args); msg != "" {
			return msg, true
		}
		if args.ID == "" {
			return "Error: 'id' is required.", true
		}
		if err := h.workflows.Delete(h.agentID, args.ID); err != nil {
			return fmt.Sprintf("Error deleting workflow: %v", err), true
		}
		log.Printf("[user=%s channel=%s] deleted workflow agent=%s id=%s", userID, channelID, h.agentID, args.ID)
		return fmt.Sprintf("Deleted workflow %s.", args.ID), true

	case "update_workflow":
		if h.headless {
			return "Error: workflows cannot update workflows from inside a scheduled tick.", true
		}
		// Parse into a raw map first so we can tell which fields were
		// actually supplied by the model (as opposed to JSON zero values,
		// which we MUST NOT interpret as "clear this field").
		var raw map[string]json.RawMessage
		if msg := unmarshalArgs(argsJSON, &raw); msg != "" {
			return msg, true
		}
		idRaw, ok := raw["id"]
		if !ok {
			return "Error: 'id' is required.", true
		}
		var id string
		if err := json.Unmarshal(idRaw, &id); err != nil || id == "" {
			return "Error: 'id' must be a non-empty string.", true
		}
		var opts workflows.UpdateOpts
		if v, ok := raw["name"]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return fmt.Sprintf("Error: 'name' must be a string: %v", err), true
			}
			opts.Name = &s
		}
		if v, ok := raw["description"]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return fmt.Sprintf("Error: 'description' must be a string: %v", err), true
			}
			opts.Description = &s
		}
		if v, ok := raw["interval"]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return fmt.Sprintf("Error: 'interval' must be a string: %v", err), true
			}
			opts.Interval = &s
		}
		if v, ok := raw["run_at_utc"]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return fmt.Sprintf("Error: 'run_at_utc' must be a string: %v", err), true
			}
			opts.RunAtUTC = &s
		}
		if v, ok := raw["prompt"]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return fmt.Sprintf("Error: 'prompt' must be a string: %v", err), true
			}
			opts.Prompt = &s
		}
		if v, ok := raw["tasks"]; ok {
			var tasks []workflows.Task
			if err := json.Unmarshal(v, &tasks); err != nil {
				return fmt.Sprintf("Error: 'tasks' must be an array of {name,prompt}: %v", err), true
			}
			opts.Tasks = &tasks
		}
		if v, ok := raw["trigger"]; ok {
			var trig workflows.Trigger
			if err := json.Unmarshal(v, &trig); err != nil {
				return fmt.Sprintf("Error: 'trigger' must be an object: %v", err), true
			}
			opts.Trigger = &trig
		}
		if v, ok := raw["enabled"]; ok {
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				return fmt.Sprintf("Error: 'enabled' must be a boolean: %v", err), true
			}
			opts.Enabled = &b
		}
		w, err := h.workflows.Update(ctx, h.agentID, id, opts)
		if err != nil {
			return fmt.Sprintf("Error updating workflow: %v", err), true
		}
		log.Printf("[user=%s channel=%s] updated workflow agent=%s id=%s pattern=%s", userID, channelID, h.agentID, w.ID, w.Pattern())
		url := h.appURL + w.ViewURL()
		return fmt.Sprintf("Updated workflow %q (id=%s, pattern=%s, every %s, enabled=%t). View: %s",
			w.Name, w.ID, w.Pattern(), w.Interval, w.Enabled, url), true

	case "call_workflow":
		var args struct {
			Agent string `json:"agent"`
			ID    string `json:"id"`
		}
		if msg := unmarshalArgs(argsJSON, &args); msg != "" {
			return msg, true
		}
		if args.ID == "" {
			return "Error: 'id' is required.", true
		}
		agent := args.Agent
		if agent == "" {
			agent = h.agentID
		}
		w, ok := h.workflows.Get(agent, args.ID)
		if !ok {
			// Try short_name lookup on the same agent.
			for _, cand := range h.workflows.List(agent) {
				if cand.ShortName == args.ID {
					w = cand
					ok = true
					break
				}
			}
		}
		if !ok {
			return fmt.Sprintf("Error: workflow %s/%s not found.", agent, args.ID), true
		}
		out, err := h.workflows.RunOnce(ctx, w.Agent, w.ID, "call_workflow:"+h.agentID)
		if err != nil {
			return fmt.Sprintf("Workflow %s/%s finished with error: %v\nResult:\n%s", w.Agent, w.ID, err, out), true
		}
		return fmt.Sprintf("Workflow %s/%s (%q) completed.\nResult:\n%s", w.Agent, w.ID, w.Name, out), true
	}
	return "", false
}
