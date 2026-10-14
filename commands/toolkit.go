package commands

import (
	"context"
	"slices"

	"github.com/justmike1/arbetern/llm"
)

// readOnlyTools are the integration tools that only read; they are what a project session may call with arbetern's access.
// Raw storage reads and http_get stay out: the state bucket holds credentials, and a fetch of any URL is an exfiltration channel.
var readOnlyTools = []string{
	ToolListOrgRepos, ToolListUserRepos, ToolListRepoTeams, ToolListRepoTeamsBulk, ToolGetFilesBulk, ToolGetFileContent,
	ToolGetRepoDefaultBranch, ToolResolveOwner, ToolSearchFiles, ToolListDirectory, ToolGetPullRequest, ToolListPullRequests,
	ToolSearchPullRequests, ToolListCommits, ToolGetCommit, ToolSearchCode, ToolSearchCodeOrg, ToolGetWorkflowRun,
	ToolSlackConversationsHistory, ToolSlackConversationsReplies, ToolFetchThreadContext, ToolGetSlackUserInfo, ToolLookupSlackUser,
	ToolLookupCVE, ToolSearchCVE,
	ToolSalesforceQuery, ToolSalesforceDescribe,
	ToolChorusListConversations, ToolChorusGetConversation, ToolChorusGetSalesQualification,
	ToolListJiraProjects, ToolSearchJiraIssues, ToolGetJiraIssue, ToolGetJiraLabelAuthor, ToolListJiraComments,
	ToolResolveJiraUser, ToolResolveJiraTeam, ToolGetJiraDashboard, ToolGetJiraFilter,
	ToolSearchConfluencePages, ToolGetConfluencePage, ToolListConfluenceSpaces,
	ToolDatadogSearchLogs, ToolDatadogLogsAggregate, ToolDatadogListMonitors, ToolDatadogGetMonitor, ToolDatadogListHosts,
	ToolDatadogGetDashboard, ToolDatadogListDashboards, ToolDatadogQueryMetrics,
	ToolAWSGetCostAndUsage, ToolAWSGetCostForecast, ToolAWSListDimensionValues,
	ToolAWSAthenaQuery, ToolAWSAthenaSchema, ToolAWSAthenaCatalogs,
	ToolAzureGetCostAndUsage, ToolAzureGetCostForecast, ToolAzureListDimensionValues,
	ToolDatabricksQuery,
	ToolClickHouseUsageCost, ToolClickHouseQuery,
	ToolFreshdeskListTickets, ToolFreshdeskGetTicket, ToolFreshdeskSearchTickets, ToolFreshdeskFindAgent, ToolFreshdeskListTicketFields,
	ToolFreshchatGetConversation, ToolFreshchatGetConversationMessages, ToolFreshworksCRMSearch, ToolFreshworksCRMGetContact, ToolFreshworksCRMGetDeal,
	ToolDocument360ListWorkspaces, ToolDocument360Search, ToolDocument360ListCategories, ToolDocument360ListArticles, ToolDocument360GetArticle,
	ToolSheetsReadRange, ToolSheetsGetInfo, ToolDriveFindFile, ToolDriveListFolders, ToolDriveReadFile,
}

// Toolkit runs the read-only integration tools of a router for callers outside its chat loops, with every configured integration allowed.
type Toolkit struct {
	r *Router
}

// ReadOnlyToolkit exposes the router's read-only tools.
func (r *Router) ReadOnlyToolkit() *Toolkit { return &Toolkit{r: r} }

func (t *Toolkit) handler() *GeneralHandler {
	h := t.r.newGeneralHandler("", nil)
	h.headless = true
	h.unrestricted = true
	return h
}

// Tools lists the read-only tools the configured integrations provide.
func (t *Toolkit) Tools() []llm.Tool {
	var out []llm.Tool
	for _, tool := range t.handler().buildTools() {
		if slices.Contains(readOnlyTools, tool.Function.Name) {
			out = append(out, tool)
		}
	}
	return out
}

// Call runs one read-only tool; caller labels the log lines.
func (t *Toolkit) Call(ctx context.Context, caller, name, args string) string {
	if !slices.Contains(readOnlyTools, name) {
		return "Error: " + name + " is not available to project sessions."
	}
	h := t.handler()
	if !slices.ContainsFunc(h.buildTools(), func(tool llm.Tool) bool { return tool.Function.Name == name }) {
		return "Error: " + name + " is not configured in this deployment."
	}
	return h.executeTool(ctx, "", caller, "", name, args)
}
