package dashboards

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/chorus"
	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/salesforce"
)

// Clients bundles the integration clients used to execute dashboard data sources.
// All fields are optional — missing clients simply mean those source types return an error.
type Clients struct {
	Jira    *atlassian.Client
	SF      *salesforce.Client
	Chorus  *chorus.Client
	Datadog *datadog.MultiClient
	GitHub  *github.Client
}

// SupportedSourceTypes returns the allow-listed DataSource.Type values the executor accepts.
// Exposed so the LLM tool can validate input before handing it to the registry.
var SupportedSourceTypes = []string{
	"jira_search",
	"salesforce_query",
	"chorus_list_conversations",
	"datadog_search_logs",
	"datadog_list_monitors",
	"confluence_search",
	"github_list_prs",
}

// SupportsType reports whether the given source type is recognised.
func SupportsType(t string) bool {
	for _, s := range SupportedSourceTypes {
		if s == t {
			return true
		}
	}
	return false
}

// ValidateSource checks that a DataSource has the required args for its type.
// It returns nil when the source is ready to execute. This is called at
// creation time so the LLM is forced to provide the required query args up
// front, rather than failing every sync with "requires args.X".
func ValidateSource(src DataSource) error {
	if !SupportsType(src.Type) {
		return fmt.Errorf("unsupported source type %q", src.Type)
	}
	required := map[string]string{
		"jira_search":         "jql",
		"salesforce_query":    "soql",
		"datadog_search_logs": "query",
		"confluence_search":   "cql",
		"github_list_prs":     "repo",
	}
	if key, ok := required[src.Type]; ok {
		if v, _ := src.Args[key].(string); strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s requires args.%s", src.Type, key)
		}
	}
	return nil
}

// clientExecutor implements Executor against live integration clients.
type clientExecutor struct{ Clients }

// NewExecutor returns an Executor that dispatches sources to the supplied integration clients.
func NewExecutor(c Clients) Executor { return &clientExecutor{Clients: c} }

// Execute dispatches a DataSource to the matching integration client.
// All source types are read-only. Unknown types return an error — they are
// validated at creation time via SupportsType, so this should not fire in practice.
func (e *clientExecutor) Execute(ctx context.Context, src DataSource) (any, error) {
	switch src.Type {
	case "jira_search":
		if e.Jira == nil || !e.Jira.Ready() {
			return nil, fmt.Errorf("jira client not ready")
		}
		jql, _ := src.Args["jql"].(string)
		if jql == "" {
			return nil, fmt.Errorf("jira_search requires args.jql")
		}
		max := intArg(src.Args, "max_results", 20)
		if max > 50 {
			max = 50
		}
		issues, err := e.Jira.SearchIssuesJQL(jql, max)
		if err != nil {
			return nil, err
		}
		return issues, nil

	case "salesforce_query":
		if e.SF == nil || !e.SF.Ready() {
			return nil, fmt.Errorf("salesforce client not ready")
		}
		soql, _ := src.Args["soql"].(string)
		if soql == "" {
			return nil, fmt.Errorf("salesforce_query requires args.soql")
		}
		res, err := e.SF.Query(soql)
		if err != nil {
			return nil, err
		}
		return res.Records, nil

	case "chorus_list_conversations":
		if e.Chorus == nil || !e.Chorus.Ready() {
			return nil, fmt.Errorf("chorus client not ready")
		}
		filter := chorus.EngagementFilter{
			MinDate:           stringArg(src.Args, "min_date"),
			MaxDate:           stringArg(src.Args, "max_date"),
			EngagementType:    stringArg(src.Args, "engagement_type"),
			ContentType:       stringArg(src.Args, "content_type"),
			ParticipantsEmail: stringArg(src.Args, "participants_email"),
			UserID:            stringArg(src.Args, "user_id"),
			TeamID:            stringArg(src.Args, "team_id"),
			EngagementID:      stringArg(src.Args, "engagement_id"),
			WithTrackers:      boolArg(src.Args, "with_trackers"),
		}
		engs, err := e.Chorus.ListEngagements(filter)
		if err != nil {
			return nil, err
		}
		return engs, nil

	case "datadog_search_logs":
		if e.Datadog == nil {
			return nil, fmt.Errorf("datadog client not configured")
		}
		query := stringArg(src.Args, "query")
		if query == "" {
			return nil, fmt.Errorf("datadog_search_logs requires args.query")
		}
		text, err := e.Datadog.SearchLogs(ctx,
			stringArg(src.Args, "site"),
			query,
			stringArg(src.Args, "from"),
			stringArg(src.Args, "to"),
			intArg(src.Args, "limit", 20),
		)
		if err != nil {
			return nil, err
		}
		return text, nil

	case "datadog_list_monitors":
		if e.Datadog == nil {
			return nil, fmt.Errorf("datadog client not configured")
		}
		text, err := e.Datadog.ListMonitors(ctx,
			stringArg(src.Args, "site"),
			stringArg(src.Args, "query"),
			intArg(src.Args, "limit", 20),
		)
		if err != nil {
			return nil, err
		}
		return text, nil

	case "confluence_search":
		if e.Jira == nil || !e.Jira.Ready() {
			return nil, fmt.Errorf("atlassian client not ready")
		}
		cql := stringArg(src.Args, "cql")
		if cql == "" {
			return nil, fmt.Errorf("confluence_search requires args.cql")
		}
		pages, err := e.Jira.SearchConfluencePages(cql, intArg(src.Args, "limit", 10))
		if err != nil {
			return nil, err
		}
		return pages, nil

	case "github_list_prs":
		if e.GitHub == nil {
			return nil, fmt.Errorf("github client not configured")
		}
		repo := stringArg(src.Args, "repo")
		if repo == "" {
			return nil, fmt.Errorf("github_list_prs requires args.repo")
		}
		owner, err := e.GitHub.ResolveOwner(ctx)
		if err != nil {
			return nil, err
		}
		state := stringArg(src.Args, "state")
		if state == "" {
			state = "all"
		}
		prs, err := e.GitHub.ListPullRequests(ctx, owner, repo, state, intArg(src.Args, "limit", 10))
		if err != nil {
			return nil, err
		}
		return prs, nil
	}
	return nil, fmt.Errorf("unsupported source type %q", src.Type)
}

// stringArg coerces an argument to string. Non-strings are stringified via JSON.
func stringArg(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// intArg reads an integer (possibly encoded as float64 from JSON) or returns def.
func intArg(m map[string]any, key string, def int) int {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return def
}

func boolArg(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}
