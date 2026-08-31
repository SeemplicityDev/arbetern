package commands

// Tool name identifiers.
//
// Each tool's wire name (the string the LLM sees and calls) is declared here
// EXACTLY ONCE as a typed constant, then referenced everywhere else:
//   - the tool definitions in buildTools (Name: …)
//   - the dispatch switch in executeTool (case …)
//   - the per-integration gating/catalogue map toolIntegration (below)
//
// This makes the names behave like a compile-checked enum: a typo becomes a
// build error instead of a silently-unroutable tool, and there is a single
// source of truth shared across the codebase (the UI "Tools" tab, the dispatch
// guard, and the LLM tool schema all resolve back to these constants).
const (
	// GitHub.
	ToolListOrgRepos         = "list_org_repos"
	ToolListUserRepos        = "list_user_repos"
	ToolListRepoTeams        = "list_repo_teams"
	ToolListRepoTeamsBulk    = "list_repo_teams_bulk"
	ToolGetFilesBulk         = "get_files_bulk"
	ToolGetFileContent       = "get_file_content"
	ToolGetRepoDefaultBranch = "get_repo_default_branch"
	ToolGetAuthenticatedUser = "get_authenticated_user"
	ToolResolveOwner         = "resolve_owner"
	ToolSearchFiles          = "search_files"
	ToolListDirectory        = "list_directory"
	ToolModifyFile           = "modify_file"
	ToolCreateFile           = "create_file"
	ToolRegexReplaceFile     = "regex_replace_file"
	ToolGetPullRequest       = "get_pull_request"
	ToolListPullRequests     = "list_pull_requests"
	ToolSearchPullRequests   = "search_pull_requests"
	ToolListCommits          = "list_commits"
	ToolGetCommit            = "get_commit"
	ToolSearchCode           = "search_code"
	ToolSearchCodeOrg        = "search_code_org"
	ToolGetWorkflowRun       = "get_workflow_run"
	ToolRerunFailedJobs      = "rerun_failed_jobs"
	ToolRerunWorkflow        = "rerun_workflow"

	// Slack.
	ToolFetchChannelContext       = "fetch_channel_context"
	ToolReplyInThread             = "reply_in_thread"
	ToolPostSlackMessage          = "post_slack_message"
	ToolSlackConversationsHistory = "slack_conversations_history"
	ToolSlackConversationsReplies = "slack_conversations_replies"
	ToolUploadSnippet             = "upload_snippet"
	ToolUploadAggregateCSV        = "upload_aggregate_csv"
	ToolFetchThreadContext        = "fetch_thread_context"
	ToolGetSlackUserInfo          = "get_slack_user_info"
	ToolLookupSlackUser           = "lookup_slack_user"

	// NVD.
	ToolLookupCVE = "lookup_cve"
	ToolSearchCVE = "search_cve"

	// Salesforce.
	ToolSalesforceQuery    = "salesforce_query"
	ToolSalesforceDescribe = "salesforce_describe"

	// Chorus.
	ToolChorusListConversations        = "chorus_list_conversations"
	ToolChorusGetConversation          = "chorus_get_conversation"
	ToolChorusCreateSalesQualification = "chorus_create_sales_qualification"
	ToolChorusGetSalesQualification    = "chorus_get_sales_qualification"
	ToolChorusWritebackCRM             = "chorus_writeback_crm"

	// Atlassian (Jira + Confluence, shared client).
	ToolCreateJiraTicket       = "create_jira_ticket"
	ToolListJiraProjects       = "list_jira_projects"
	ToolSearchJiraIssues       = "search_jira_issues"
	ToolGetJiraIssue           = "get_jira_issue"
	ToolGetJiraLabelAuthor     = "get_jira_label_author"
	ToolUpdateJiraIssue        = "update_jira_issue"
	ToolAssignJiraActiveSprint = "assign_jira_active_sprint"
	ToolAssignJiraTeam         = "assign_jira_team"
	ToolAddJiraComment         = "add_jira_comment"
	ToolListJiraComments       = "list_jira_comments"
	ToolLinkJiraIssues         = "link_jira_issues"
	ToolResolveJiraUser        = "resolve_jira_user"
	ToolResolveJiraTeam        = "resolve_jira_team"
	ToolGetJiraDashboard       = "get_jira_dashboard"
	ToolGetJiraFilter          = "get_jira_filter"
	ToolSearchConfluencePages  = "search_confluence_pages"
	ToolGetConfluencePage      = "get_confluence_page"
	ToolListConfluenceSpaces   = "list_confluence_spaces"
	ToolCreateConfluencePage   = "create_confluence_page"

	// Datadog.
	ToolDatadogSearchLogs     = "datadog_search_logs"
	ToolDatadogLogsAggregate  = "datadog_logs_aggregate"
	ToolDatadogListMonitors   = "datadog_list_monitors"
	ToolDatadogGetMonitor     = "datadog_get_monitor"
	ToolDatadogListHosts      = "datadog_list_hosts"
	ToolDatadogGetDashboard   = "datadog_get_dashboard"
	ToolDatadogListDashboards = "datadog_list_dashboards"
	ToolDatadogQueryMetrics   = "datadog_query_metrics"

	// AWS.
	ToolAWSGetCostAndUsage     = "aws_get_cost_and_usage"
	ToolAWSGetCostForecast     = "aws_get_cost_forecast"
	ToolAWSListDimensionValues = "aws_list_dimension_values"
	ToolAWSS3PutObject         = "aws_s3_put_object"
	ToolAWSS3GetObject         = "aws_s3_get_object"
	ToolAWSS3ListObjects       = "aws_s3_list_objects"

	// Azure.
	ToolAzureGetCostAndUsage     = "azure_get_cost_and_usage"
	ToolAzureGetCostForecast     = "azure_get_cost_forecast"
	ToolAzureListDimensionValues = "azure_list_dimension_values"

	// Databricks.
	ToolDatabricksQuery = "databricks_query"

	// ClickHouse.
	ToolClickHouseUsageCost = "clickhouse_usage_cost"
	ToolClickHouseQuery     = "clickhouse_query"

	// Freshworks (Freshdesk / Freshchat / CRM).
	ToolFreshdeskListTickets             = "freshdesk_list_tickets"
	ToolFreshdeskGetTicket               = "freshdesk_get_ticket"
	ToolFreshdeskSearchTickets           = "freshdesk_search_tickets"
	ToolFreshdeskFindAgent               = "freshdesk_find_agent"
	ToolFreshdeskListTicketFields        = "freshdesk_list_ticket_fields"
	ToolFreshchatGetConversation         = "freshchat_get_conversation"
	ToolFreshchatGetConversationMessages = "freshchat_get_conversation_messages"
	ToolFreshworksCRMSearch              = "freshworks_crm_search"
	ToolFreshworksCRMGetContact          = "freshworks_crm_get_contact"
	ToolFreshworksCRMGetDeal             = "freshworks_crm_get_deal"

	// Google Drive / Sheets.
	ToolSheetsAppendRow  = "sheets_append_row"
	ToolSheetsReadRange  = "sheets_read_range"
	ToolSheetsGetInfo    = "sheets_get_spreadsheet_info"
	ToolDriveFindFile    = "drive_find_file"
	ToolDriveListFolders = "drive_list_folders"
	ToolDriveReadFile    = "drive_read_file"

	// Universal utility (no integration card).
	ToolHTTPGet = "http_get"
)

// toolIntegration maps every integration-scoped tool to the integration that
// owns it. It is the SINGLE catalogue used both for:
//   - the dispatch-side gating guard (toolDenied): a tool whose integration is
//     not in the caller-agent's allowlist is refused before it reaches a
//     connector; and
//   - the home page "Tools" tab (ToolsForIntegration): every connector's tools
//     are listed straight from here.
//
// Connectors that are open to every agent (GitHub, Slack, Azure) are listed
// here too, mapped to their integration id — because those ids are absent from
// restrictedIntegrations, canUseIntegration returns true for them, so listing
// them changes no gating decision while giving the UI a single source of truth.
// Truly universal utility tools (http_get, and the dashboards/workflows tools
// handled before this switch) are intentionally omitted: they belong to no
// integration card and must never be gated.
var toolIntegration = map[string]string{
	// GitHub (open to all agents).
	ToolListOrgRepos:         integrationGitHub,
	ToolListUserRepos:        integrationGitHub,
	ToolListRepoTeams:        integrationGitHub,
	ToolListRepoTeamsBulk:    integrationGitHub,
	ToolGetFilesBulk:         integrationGitHub,
	ToolGetFileContent:       integrationGitHub,
	ToolGetRepoDefaultBranch: integrationGitHub,
	ToolGetAuthenticatedUser: integrationGitHub,
	ToolResolveOwner:         integrationGitHub,
	ToolSearchFiles:          integrationGitHub,
	ToolListDirectory:        integrationGitHub,
	ToolModifyFile:           integrationGitHub,
	ToolCreateFile:           integrationGitHub,
	ToolRegexReplaceFile:     integrationGitHub,
	ToolGetPullRequest:       integrationGitHub,
	ToolListPullRequests:     integrationGitHub,
	ToolListCommits:          integrationGitHub,
	ToolGetCommit:            integrationGitHub,
	ToolSearchPullRequests:   integrationGitHub,
	ToolSearchCode:           integrationGitHub,
	ToolSearchCodeOrg:        integrationGitHub,
	ToolGetWorkflowRun:       integrationGitHub,
	ToolRerunFailedJobs:      integrationGitHub,
	ToolRerunWorkflow:        integrationGitHub,

	// Slack (open to all agents).
	ToolFetchChannelContext:       integrationSlack,
	ToolReplyInThread:             integrationSlack,
	ToolPostSlackMessage:          integrationSlack,
	ToolSlackConversationsHistory: integrationSlack,
	ToolSlackConversationsReplies: integrationSlack,
	ToolUploadSnippet:             integrationSlack,
	ToolUploadAggregateCSV:        integrationSlack,
	ToolFetchThreadContext:        integrationSlack,
	ToolGetSlackUserInfo:          integrationSlack,
	ToolLookupSlackUser:           integrationSlack,

	// NVD.
	ToolLookupCVE: integrationNVD,
	ToolSearchCVE: integrationNVD,

	// Salesforce.
	ToolSalesforceQuery:    integrationSalesforce,
	ToolSalesforceDescribe: integrationSalesforce,

	// Chorus.
	ToolChorusListConversations:        integrationChorus,
	ToolChorusGetConversation:          integrationChorus,
	ToolChorusCreateSalesQualification: integrationChorus,
	ToolChorusGetSalesQualification:    integrationChorus,
	ToolChorusWritebackCRM:             integrationChorus,

	// Atlassian (Jira + Confluence).
	ToolCreateJiraTicket:       integrationAtlassian,
	ToolListJiraProjects:       integrationAtlassian,
	ToolSearchJiraIssues:       integrationAtlassian,
	ToolGetJiraIssue:           integrationAtlassian,
	ToolGetJiraLabelAuthor:     integrationAtlassian,
	ToolUpdateJiraIssue:        integrationAtlassian,
	ToolAssignJiraActiveSprint: integrationAtlassian,
	ToolAssignJiraTeam:         integrationAtlassian,
	ToolAddJiraComment:         integrationAtlassian,
	ToolListJiraComments:       integrationAtlassian,
	ToolLinkJiraIssues:         integrationAtlassian,
	ToolResolveJiraUser:        integrationAtlassian,
	ToolResolveJiraTeam:        integrationAtlassian,
	ToolGetJiraDashboard:       integrationAtlassian,
	ToolGetJiraFilter:          integrationAtlassian,
	ToolSearchConfluencePages:  integrationAtlassian,
	ToolGetConfluencePage:      integrationAtlassian,
	ToolListConfluenceSpaces:   integrationAtlassian,
	ToolCreateConfluencePage:   integrationAtlassian,

	// Datadog.
	ToolDatadogSearchLogs:     integrationDatadog,
	ToolDatadogLogsAggregate:  integrationDatadog,
	ToolDatadogListMonitors:   integrationDatadog,
	ToolDatadogGetMonitor:     integrationDatadog,
	ToolDatadogListHosts:      integrationDatadog,
	ToolDatadogGetDashboard:   integrationDatadog,
	ToolDatadogListDashboards: integrationDatadog,
	ToolDatadogQueryMetrics:   integrationDatadog,

	// AWS.
	ToolAWSGetCostAndUsage:     integrationAWS,
	ToolAWSGetCostForecast:     integrationAWS,
	ToolAWSListDimensionValues: integrationAWS,
	ToolAWSS3PutObject:         integrationAWS,
	ToolAWSS3GetObject:         integrationAWS,
	ToolAWSS3ListObjects:       integrationAWS,

	// Azure (open to all agents).
	ToolAzureGetCostAndUsage:     integrationAzure,
	ToolAzureGetCostForecast:     integrationAzure,
	ToolAzureListDimensionValues: integrationAzure,

	// Databricks.
	ToolDatabricksQuery: integrationDatabricks,

	// ClickHouse Cloud billing.
	ToolClickHouseUsageCost: integrationClickHouse,
	ToolClickHouseQuery:     integrationClickHouse,

	// Freshworks (Freshdesk / Freshchat / CRM).
	ToolFreshdeskListTickets:             integrationFreshworks,
	ToolFreshdeskGetTicket:               integrationFreshworks,
	ToolFreshdeskSearchTickets:           integrationFreshworks,
	ToolFreshdeskFindAgent:               integrationFreshworks,
	ToolFreshdeskListTicketFields:        integrationFreshworks,
	ToolFreshchatGetConversation:         integrationFreshworks,
	ToolFreshchatGetConversationMessages: integrationFreshworks,
	ToolFreshworksCRMSearch:              integrationFreshworks,
	ToolFreshworksCRMGetContact:          integrationFreshworks,
	ToolFreshworksCRMGetDeal:             integrationFreshworks,

	// Google Drive / Sheets.
	ToolSheetsAppendRow:  integrationGoogle,
	ToolSheetsReadRange:  integrationGoogle,
	ToolSheetsGetInfo:    integrationGoogle,
	ToolDriveFindFile:    integrationGoogle,
	ToolDriveListFolders: integrationGoogle,
	ToolDriveReadFile:    integrationGoogle,
}
