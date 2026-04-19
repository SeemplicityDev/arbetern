package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/justmike1/arbetern/dashboards"
	"github.com/justmike1/arbetern/llm"
)

// dashboardTools returns the LLM tools for creating, listing, and deleting
// dashboards. The tools are only exposed when a registry is configured.
func (h *GeneralHandler) dashboardTools() []llm.Tool {
	if h.dashboards == nil {
		return nil
	}
	supported := strings.Join(dashboards.SupportedSourceTypes, ", ")
	return []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "create_dashboard",
				Description: "Create a recurring data dashboard owned by the current agent. The dashboard periodically fetches data from the allow-listed integration sources and persists each run as JSON on disk, which is rendered as an HTML page at /" + h.agentID + "/dashboard/<id>. Use this when the user asks to 'create a dashboard', 'build me a view', or 'sync X every N minutes'. Compose one DataSource per distinct data feed the user is asking about — e.g. a Salesforce SOQL query per object, a Jira JQL search per board, a Chorus filter per engagement slice. Each source MUST be from this allow-list: " + supported + ". Give each source a short human label via its 'name' so the dashboard UI can title its section. sync_interval accepts Go duration strings like '5m', '30s', '1h'. Returns the dashboard id, short_name, and view URL which you MUST include in your reply to the user.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"name":{"type":"string","description":"Human title of the dashboard (e.g. 'PayPal customer 360')."},
						"short_name":{"type":"string","description":"Short slug shown on the agent card (lowercase, hyphenated, <=24 chars). E.g. 'paypal-360'."},
						"description":{"type":"string","description":"One-sentence summary of what the dashboard shows."},
						"sync_interval":{"type":"string","description":"Go duration between refreshes. Minimum 30s, maximum 24h, default 5m. Examples: '5m', '15m', '1h'."},
						"sources":{
							"type":"array",
							"description":"Ordered list of data feeds the dashboard refreshes on every sync.",
							"items":{
								"type":"object",
								"properties":{
									"type":{"type":"string","enum":["jira_search","salesforce_query","chorus_list_conversations","datadog_search_logs","datadog_list_monitors","confluence_search","github_list_prs"],"description":"Which integration to call. Each type accepts a specific 'args' shape — see below."},
									"name":{"type":"string","description":"Short label for this source, shown as the section title on the rendered dashboard."},
									"args":{
										"type":"object",
										"description":"Type-specific arguments. jira_search: {jql: string, max_results?: int}. salesforce_query: {soql: string}. chorus_list_conversations: {min_date?, max_date?, engagement_type?, content_type?, participants_email?, user_id?, team_id?, engagement_id?, with_trackers?:bool}. datadog_search_logs: {query: string, site?: 'us'|'eu', from?: string, to?: string, limit?: int}. datadog_list_monitors: {site?: 'us'|'eu', query?: string, limit?: int}. confluence_search: {cql: string, limit?: int}. github_list_prs: {repo: string, state?: 'open'|'closed'|'all', limit?: int}."
									}
								},
								"required":["type","name","args"]
							},
							"minItems":1
						}
					},
					"required":["name","short_name","sources"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "list_dashboards",
				Description: "List the dashboards currently owned by this agent, returning their ids, names, short names, sync intervals, and last-sync timestamps. Use this when the user asks 'what dashboards do you have', 'show me my dashboards', or before calling delete_dashboard to find the correct id.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "delete_dashboard",
				Description: "Stop the background sync for a dashboard, remove its stored JSON, and make its view URL return 404. Requires the dashboard id (use list_dashboards to find it). This is irreversible.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"id":{"type":"string","description":"Dashboard id (the 16-hex segment from the view URL)."}
					},
					"required":["id"]
				}`),
			},
		},
	}
}

// executeDashboardTool dispatches the dashboard-related LLM tool calls.
// Returns ("", false) when the tool name is not a dashboard tool, letting the
// main switch fall through.
func (h *GeneralHandler) executeDashboardTool(ctx context.Context, userID, channelID, name, argsJSON string) (string, bool) {
	if h.dashboards == nil {
		return "", false
	}
	switch name {
	case "create_dashboard":
		var args struct {
			Name         string                  `json:"name"`
			ShortName    string                  `json:"short_name"`
			Description  string                  `json:"description"`
			SyncInterval string                  `json:"sync_interval"`
			Sources      []dashboards.DataSource `json:"sources"`
		}
		if msg := unmarshalArgs(argsJSON, &args); msg != "" {
			return msg, true
		}
		if args.Name == "" || len(args.Sources) == 0 {
			return "Error: 'name' and at least one source are required.", true
		}
		for _, s := range args.Sources {
			// ValidateSource covers both unknown types and missing required args.
			if err := dashboards.ValidateSource(s); err != nil {
				return fmt.Sprintf("Error: source %q (%s) invalid: %v. Allowed types: %s. Re-call create_dashboard with the required args populated.", s.Name, s.Type, err, strings.Join(dashboards.SupportedSourceTypes, ", ")), true
			}
		}
		interval := args.SyncInterval
		if interval == "" {
			interval = dashboards.DefaultSyncInterval.String()
		} else if _, err := time.ParseDuration(interval); err != nil {
			return fmt.Sprintf("Error: invalid sync_interval %q: %v", interval, err), true
		}
		d, err := h.dashboards.Create(ctx, h.agentID, userID, args.Name, args.ShortName, args.Description, interval, args.Sources)
		if err != nil {
			return fmt.Sprintf("Error creating dashboard: %v", err), true
		}
		log.Printf("[user=%s channel=%s] created dashboard agent=%s id=%s", userID, channelID, h.agentID, d.ID)
		url := h.appURL + d.ViewURL()
		return fmt.Sprintf("Created dashboard %q (id=%s, short=%s, sync=%s). View: %s", d.Name, d.ID, d.ShortName, d.SyncInterval, url), true

	case "list_dashboards":
		list := h.dashboards.List(h.agentID)
		if len(list) == 0 {
			return "No dashboards are currently registered for this agent.", true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Dashboards for %s (%d):\n", h.agentID, len(list))
		for _, d := range list {
			lastSync := d.LastSync
			if lastSync == "" {
				lastSync = "pending"
			}
			fmt.Fprintf(&b, "- %s — %s (short=%s, every %s, last sync %s)\n  %s\n", d.ID, d.Name, d.ShortName, d.SyncInterval, lastSync, h.appURL+d.ViewURL())
		}
		return b.String(), true

	case "delete_dashboard":
		var args struct {
			ID string `json:"id"`
		}
		if msg := unmarshalArgs(argsJSON, &args); msg != "" {
			return msg, true
		}
		if args.ID == "" {
			return "Error: 'id' is required.", true
		}
		if err := h.dashboards.Delete(h.agentID, args.ID); err != nil {
			return fmt.Sprintf("Error deleting dashboard: %v", err), true
		}
		log.Printf("[user=%s channel=%s] deleted dashboard agent=%s id=%s", userID, channelID, h.agentID, args.ID)
		return fmt.Sprintf("Deleted dashboard %s.", args.ID), true
	}
	return "", false
}

func unmarshalArgs(argsJSON string, out any) string {
	if err := json.Unmarshal([]byte(argsJSON), out); err != nil {
		return fmt.Sprintf("Error parsing arguments: %v", err)
	}
	return ""
}
