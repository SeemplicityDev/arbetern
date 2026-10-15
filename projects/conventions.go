package projects

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/internal/store"
)

const (
	descriptionSession = "session"
	maxTemplateChars   = 200
	maxTitleChars      = 200
	maxTicketSummary   = 250
	maxTicketText      = 4000
	maxTicketConfig    = 8 << 10
	maxSessionFields   = 4 << 10
	maxSessionSummary  = 12000
	maxBranchTitle     = 50
	maxHeadlineChars   = 300
)

var (
	jiraProjectRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,19}$`)
	jiraFieldRe    = regexp.MustCompile(`^[a-z][A-Za-z0-9_]{0,63}$`)
	issueKeyRe     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,19}-[0-9]{1,10}$`)
	placeholderRe  = regexp.MustCompile(`\{[a-z_]+\}`)
	conventionalRe = regexp.MustCompile(`^[a-z]+(\([^()]*\))?!?:\s*`)
	// Fields arbetern sets itself; configuration and sessions cannot override them.
	coreTicketFields = []string{"project", "issuetype", "summary", "description", "assignee", "reporter", "labels"}
)

// Conventions are the rules a repository sets for the pull requests a project opens.
type Conventions struct {
	Ticket       *Ticket `json:"ticket,omitempty"`        // an issue filed in the deployment's Jira with every pull request
	Branch       string  `json:"branch,omitempty"`        // pull request branch from {ticket}, {title}, {task} and {project}; empty keeps the session's branch
	Title        string  `json:"title,omitempty"`         // pull request title from {title} and {ticket}; empty uses the session's title
	TitlePattern string  `json:"title_pattern,omitempty"` // RE2 the session's title must match
	Description  string  `json:"description,omitempty"`   // "session" posts the session's summary as the whole description
}

// Ticket configures the Jira issue a project files with each pull request.
type Ticket struct {
	Project       string                    `json:"project"`
	IssueType     string                    `json:"issue_type,omitempty"` // default Task
	AssigneeID    string                    `json:"assignee_id,omitempty"`
	Labels        []string                  `json:"labels,omitempty"`
	Fields        map[string]any            `json:"fields,omitempty"`         // further create fields, sent as given
	SiteFields    map[string]map[string]any `json:"site_fields,omitempty"`    // merged into fields for the series' Datadog site
	SessionFields []string                  `json:"session_fields,omitempty"` // fields the session may set in its report, such as an option it looked up
	ActiveSprint  bool                      `json:"active_sprint,omitempty"`  // place each issue in the project's active sprint
	DoneOnMerge   bool                      `json:"done_on_merge,omitempty"`  // move each issue to Done once its pull request merges
}

func (c *Conventions) normalize() error {
	c.Branch, c.Title = strings.TrimSpace(c.Branch), strings.TrimSpace(c.Title)
	c.TitlePattern, c.Description = strings.TrimSpace(c.TitlePattern), strings.TrimSpace(c.Description)
	if c.Ticket != nil {
		if err := c.Ticket.normalize(); err != nil {
			return err
		}
	}
	ticket := c.Ticket != nil
	if err := checkTemplate("conventions.branch", c.Branch, []string{"ticket", "title", "task", "project"}, ticket); err != nil {
		return err
	}
	if c.Branch != "" {
		if !strings.Contains(c.Branch, "{task}") && !strings.Contains(c.Branch, "{ticket}") {
			return invalidf("conventions.branch must contain {ticket} or {task}, so every pull request gets a branch of its own")
		}
		if sample := renderTemplate(c.Branch, map[string]string{"ticket": "ABC-12345", "title": "a-change", "task": "0123456789abcdef", "project": "a-project"}); !validBranchName(sample) {
			return invalidf("conventions.branch does not make a valid branch name (%q)", sample)
		}
	}
	if err := checkTemplate("conventions.title", c.Title, []string{"title", "ticket"}, ticket); err != nil {
		return err
	}
	if c.Title != "" && !strings.Contains(c.Title, "{title}") {
		return invalidf("conventions.title must contain {title}")
	}
	if c.TitlePattern != "" {
		if len(c.TitlePattern) > 500 {
			return invalidf("conventions.title_pattern is longer than 500 characters")
		}
		if _, err := regexp.Compile(c.TitlePattern); err != nil {
			return invalidf("conventions.title_pattern: %v", err)
		}
	}
	switch c.Description {
	case "", descriptionSession:
	default:
		return invalidf("conventions.description must be empty or %q", descriptionSession)
	}
	return nil
}

func checkTemplate(name, tpl string, allowed []string, ticket bool) error {
	if len(tpl) > maxTemplateChars {
		return invalidf("%s is longer than %d characters", name, maxTemplateChars)
	}
	for _, ph := range placeholderRe.FindAllString(tpl, -1) {
		key := strings.Trim(ph, "{}")
		switch {
		case !slices.Contains(allowed, key):
			return invalidf("%s: unknown placeholder %s", name, ph)
		case key == "ticket" && !ticket:
			return invalidf("%s uses {ticket}, which needs conventions.ticket", name)
		}
	}
	return nil
}

func renderTemplate(tpl string, values map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(tpl, func(ph string) string { return values[strings.Trim(ph, "{}")] })
}

func (t *Ticket) normalize() error {
	t.Project, t.IssueType, t.AssigneeID = strings.TrimSpace(t.Project), strings.TrimSpace(t.IssueType), strings.TrimSpace(t.AssigneeID)
	if t.IssueType == "" {
		t.IssueType = "Task"
	}
	if !jiraProjectRe.MatchString(t.Project) {
		return invalidf("conventions.ticket.project %q is not a Jira project key", t.Project)
	}
	if err := checkText("conventions.ticket.issue_type", t.IssueType, 1, 50); err != nil {
		return err
	}
	if err := checkText("conventions.ticket.assignee_id", t.AssigneeID, 0, 128); err != nil {
		return err
	}
	if len(t.Labels) > 10 {
		return invalidf("conventions.ticket.labels holds at most 10 labels")
	}
	for _, l := range t.Labels {
		if l == "" || len(l) > 64 || strings.ContainsFunc(l, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }) {
			return invalidf("conventions.ticket.labels: %q is not a Jira label", l)
		}
	}
	if err := checkFieldNames("conventions.ticket.fields", slices.Collect(maps.Keys(t.Fields))); err != nil {
		return err
	}
	for site, fields := range t.SiteFields {
		if s, err := normalizeSite(site); err != nil || s != site {
			return invalidf("conventions.ticket.site_fields: %q is not a Datadog site", site)
		}
		if err := checkFieldNames("conventions.ticket.site_fields."+site, slices.Collect(maps.Keys(fields))); err != nil {
			return err
		}
	}
	if len(t.SessionFields) > 10 {
		return invalidf("conventions.ticket.session_fields holds at most 10 fields")
	}
	if err := checkFieldNames("conventions.ticket.session_fields", t.SessionFields); err != nil {
		return err
	}
	if raw, err := json.Marshal(t); err != nil || len(raw) > maxTicketConfig {
		return invalidf("conventions.ticket is larger than %d bytes", maxTicketConfig)
	}
	return nil
}

func checkFieldNames(name string, keys []string) error {
	for _, k := range keys {
		switch {
		case !jiraFieldRe.MatchString(k):
			return invalidf("%s: %q is not a Jira field id", name, k)
		case slices.Contains(coreTicketFields, k):
			return invalidf("%s: arbetern sets %s itself", name, k)
		}
	}
	return nil
}

// branchTitle is the session's title as a branch name segment, without a conventional-commit type prefix.
func branchTitle(title string) string {
	slug := store.Slugify(conventionalRe.ReplaceAllString(strings.TrimSpace(title), ""), "change")
	if len(slug) > maxBranchTitle {
		slug = strings.TrimRight(slug[:maxBranchTitle], "-")
	}
	return slug
}

// prBranch is the branch a project's pull request is opened from.
func prBranch(p *Project, t *Task, o Outcome) string {
	tpl := p.Conventions.Branch
	if tpl == "" {
		return o.Branch
	}
	return renderTemplate(tpl, map[string]string{
		"ticket":  t.Ticket,
		"title":   branchTitle(o.Title),
		"task":    t.ID,
		"project": store.Slugify(p.Name, "project"),
	})
}

// prTitle is the pull request title: the session's title, laid out by the project's conventions.
func prTitle(p *Project, t *Task, title string) string {
	title = oneLine(title, 120)
	if tpl := p.Conventions.Title; tpl != "" {
		title = renderTemplate(tpl, map[string]string{"title": title, "ticket": t.Ticket})
	}
	return oneLine(cleanPRText(oneLine(title, maxTitleChars), maxTitleChars), maxTitleChars)
}

// TicketFiler files and comments on issues in the deployment's Jira.
type TicketFiler interface {
	FileTicket(ctx context.Context, req TicketRequest) (key, url string, err error)
	CommentTicket(ctx context.Context, key, text string) error
	AddToActiveSprint(ctx context.Context, project, key string) error
	ResolveTicket(ctx context.Context, key string) error
}

// TicketRequest is one issue to file.
type TicketRequest struct {
	Project, IssueType, Summary, Description, AssigneeID string
	Labels                                               []string
	Fields                                               map[string]any
	SessionFields                                        []string // keys of Fields the session set, which Jira's refusal goes back to
	ActiveSprint                                         bool
}

type refusedFieldsError struct {
	fields []string
	err    error
}

func (e *refusedFieldsError) Error() string {
	return "jira refused " + strings.Join(e.fields, ", ") + ": " + e.err.Error()
}

func (e *refusedFieldsError) Unwrap() error { return e.err }

// defuseMentions keeps session text from @-mentioning Jira users: the issue text turns @[Name](accountId) into a mention.
func defuseMentions(s string) string { return strings.ReplaceAll(s, "@[", "@"+zeroWidthSpace+"[") }

type jiraTickets struct {
	c *atlassian.Client
}

// JiraTickets files project tickets through the deployment's Jira client.
func JiraTickets(c *atlassian.Client) TicketFiler {
	if c == nil {
		return nil
	}
	return jiraTickets{c: c}
}

func (j jiraTickets) FileTicket(_ context.Context, req TicketRequest) (string, string, error) {
	issue, err := j.c.CreateIssueInProject(atlassian.CreateIssueInput{
		Project: req.Project, IssueType: req.IssueType, Summary: req.Summary, Description: req.Description,
		AssigneeID: req.AssigneeID, Labels: req.Labels, Fields: req.Fields,
	})
	if err != nil {
		return "", "", err
	}
	return issue.Key, issue.Browse, nil
}

func (j jiraTickets) AddToActiveSprint(_ context.Context, project, key string) error {
	sprint, err := j.c.GetActiveSprintForProject(project)
	if err != nil {
		return fmt.Errorf("finding the active sprint of %s: %w", project, err)
	}
	if sprint == nil {
		return fmt.Errorf("%s has no active sprint", project)
	}
	if err := j.c.SetSprintField(key, sprint.ID); err != nil {
		return fmt.Errorf("adding it to sprint %s: %w", sprint.Name, err)
	}
	return nil
}

func (j jiraTickets) ResolveTicket(_ context.Context, key string) error {
	current, transitions, err := j.c.IssueTransitions(key)
	switch {
	case err != nil:
		return err
	case current == "done":
		return nil
	}
	// Several workflow steps end in a done category, such as won't do or duplicate; only the one named Done fits a merged fix.
	for _, t := range transitions {
		if t.Category == "done" && strings.EqualFold(t.To, "done") {
			return j.c.TransitionIssue(key, t.ID)
		}
	}
	return fmt.Errorf("%s has no transition to Done", key)
}

func (j jiraTickets) CommentTicket(_ context.Context, key, text string) error {
	_, err := j.c.AddComment(key, text)
	return err
}

// ticketArg is what a session reports for the issue filed with its pull request.
type ticketArg struct {
	Summary     string                     `json:"summary"`
	Description string                     `json:"description"`
	Fields      map[string]json.RawMessage `json:"fields"`
}

func (a *ticketArg) check(tc *Ticket) (summary, description string, fields map[string]any, problem string) {
	if a == nil {
		return "", "", nil, "ticket is required when status is fixed: pass a one-line summary for the issue arbetern files with the pull request."
	}
	summary, description = oneLine(stripControl(a.Summary), 4*maxTicketSummary), strings.TrimSpace(stripControl(a.Description))
	if err := checkText("ticket.summary", summary, 1, maxTicketSummary); err != nil {
		return "", "", nil, err.Error() + "."
	}
	if err := checkText("ticket.description", description, 0, maxTicketText); err != nil {
		return "", "", nil, err.Error() + "."
	}
	size := 0
	fields = map[string]any{}
	for k, raw := range a.Fields {
		if !slices.Contains(tc.SessionFields, k) {
			allowed := "none"
			if len(tc.SessionFields) > 0 {
				allowed = strings.Join(tc.SessionFields, ", ")
			}
			return "", "", nil, fmt.Sprintf("ticket.fields: %q may not be set by a session; allowed: %s.", oneLine(k, 64), allowed)
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return "", "", nil, fmt.Sprintf("ticket.fields.%s is not valid JSON.", k)
		}
		size += len(raw)
		fields[k] = v
	}
	if size > maxSessionFields {
		return "", "", nil, fmt.Sprintf("ticket.fields is larger than %d bytes.", maxSessionFields)
	}
	return summary, description, fields, ""
}

// fileTicket files the issue for a task. Jira names the fields it refuses, and those are dropped for up to two more tries,
// so a wrong option loses one field rather than the ticket.
func fileTicket(ctx context.Context, filer TicketFiler, req TicketRequest) (key, url string, dropped []string, err error) {
	fields := maps.Clone(req.Fields)
	for attempt := 0; attempt < 3; attempt++ {
		req.Fields = fields
		if key, url, err = filer.FileTicket(ctx, req); err == nil {
			if !issueKeyRe.MatchString(key) {
				return "", "", dropped, fmt.Errorf("unexpected issue key %q from Jira", oneLine(key, 40))
			}
			return key, url, dropped, nil
		}
		var refused []string
		for k := range fields {
			if regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(k) + `($|[^A-Za-z0-9_])`).MatchString(err.Error()) {
				refused = append(refused, k)
			}
		}
		if len(refused) == 0 {
			break
		}
		sort.Strings(refused)
		if own := slices.DeleteFunc(slices.Clone(refused), func(k string) bool { return !slices.Contains(req.SessionFields, k) }); len(own) > 0 {
			return "", "", dropped, &refusedFieldsError{fields: own, err: err}
		}
		fields = maps.Clone(fields)
		for _, k := range refused {
			delete(fields, k)
		}
		dropped = append(dropped, refused...)
	}
	return "", "", dropped, err
}

// ticketFields are the create fields of a task's issue: the project's, its site's, then the session's.
func ticketFields(tc *Ticket, sites []string, session map[string]any) map[string]any {
	out := maps.Clone(tc.Fields)
	if out == nil {
		out = map[string]any{}
	}
	// A group seen on two sites gets both sites' list values, such as both regions; a scalar keeps the first site's.
	bySite := map[string]any{}
	for _, site := range sites {
		for k, v := range tc.SiteFields[site] {
			prev, seen := bySite[k]
			if !seen {
				bySite[k] = v
				continue
			}
			a, okA := prev.([]any)
			b, okB := v.([]any)
			if okA && okB {
				a = slices.Clone(a)
				for _, x := range b {
					if !slices.ContainsFunc(a, func(y any) bool { return reflect.DeepEqual(x, y) }) {
						a = append(a, x)
					}
				}
				bySite[k] = a
			}
		}
	}
	maps.Copy(out, bySite)
	maps.Copy(out, session)
	return out
}

func taskSites(p *Project, g Group) []string {
	if g.Site != "" {
		return []string{g.Site}
	}
	return p.Signal.sites()
}

// withTicket fills {ticket} and {ticket_url} in the session's text with the issue filed for the task.
func withTicket(text string, t *Task) string {
	if t.Ticket == "" {
		return text
	}
	return strings.NewReplacer("{ticket}", t.Ticket, "{ticket_url}", t.TicketURL).Replace(text)
}

func ticketMarkdown(t *Task) string {
	if strings.HasPrefix(t.TicketURL, "https://") {
		return "[" + t.Ticket + "](" + t.TicketURL + ")"
	}
	return t.Ticket
}
