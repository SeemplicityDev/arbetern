package github

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/justmike1/arbetern/internal/origin"
)

const (
	prMarkerPrefix = "<!-- arbetern"
	unresolvedUser = "an unresolved user"
)

var (
	prMarkerRe       = regexp.MustCompile(`<!-- arbetern((?: [a-z]+=[^\s>]+)*) -->`)
	requesterRe      = regexp.MustCompile(`(?:Automated|requested) via [A-Za-z][A-Za-z ]{0,40}? by (.+?)[._]*(?:\r?\n|$)`)
	workflowPhraseRe = regexp.MustCompile(`via the "([^"\n]{1,200})" workflow`)
	slackMentionRe   = regexp.MustCompile(`\s*\(?<@([A-Z0-9]+)(?:\|[^>]*)?>\)?`)
	workflowNameRepl = strings.NewReplacer(`"`, "'", "\n", " ", "\r", " ")
)

// entryPhrases names the entry paths a person asks through; any other source reads as Slack.
var entryPhrases = map[origin.Source]string{
	origin.Chat:      "the web chat",
	origin.Dashboard: "a dashboard render",
}

// Attribution says what is behind a pull request arbetern opened: the agent, the entry path, and the person, workflow or project it acted for.
type Attribution struct {
	Agent        string
	Source       origin.Source
	UserID       string // Slack member id of the person who asked
	UserName     string
	WorkflowID   string
	WorkflowName string
	ProjectID    string
	TaskID       string
}

// Marker renders the hidden comment that ends every pull request body arbetern writes; empty attributes are left out.
func (a Attribution) Marker() string {
	var b strings.Builder
	b.WriteString(prMarkerPrefix)
	for _, kv := range [][2]string{
		{"agent", a.Agent}, {"source", string(a.Source)}, {"user", a.UserID},
		{"workflow", markerValue(a.WorkflowID)}, {"project", markerValue(a.ProjectID)}, {"task", markerValue(a.TaskID)},
	} {
		if v := strings.TrimSpace(kv[1]); v != "" {
			b.WriteString(" " + kv[0] + "=" + v)
		}
	}
	b.WriteString(" -->")
	return b.String()
}

// Phrase names the entry path and who asked, as it reads after "via" in the visible attribution: a workflow is named itself, never its owner.
func (a Attribution) Phrase() string {
	if a.Source == origin.Workflow {
		name := strings.TrimSpace(workflowNameRepl.Replace(a.WorkflowName))
		if name == "" {
			return "a workflow"
		}
		return `the "` + name + `" workflow`
	}
	entry, ok := entryPhrases[a.Source]
	if !ok {
		entry = "Slack"
	}
	return entry + " by " + a.requester()
}

// GitHub does not render <@U…>, so the name leads and the mention stays as the key back to the Slack profile.
func (a Attribution) requester() string {
	id, name := strings.TrimSpace(a.UserID), strings.TrimSpace(a.UserName)
	switch {
	case id == "" && name == "":
		return unresolvedUser
	case id == "":
		return name
	case name == "":
		return "<@" + id + ">"
	}
	return name + " (<@" + id + ">)"
}

// ParseAttribution reads back what Marker and Phrase wrote into a pull request body; bodies older than the marker yield the requester only.
func ParseAttribution(body string) Attribution {
	attrs := markerAttrs(body)
	a := Attribution{
		Agent:      attrs["agent"],
		Source:     origin.Source(attrs["source"]),
		WorkflowID: attrs["workflow"],
		ProjectID:  attrs["project"],
		TaskID:     attrs["task"],
	}
	switch a.Source {
	case origin.Project:
	case origin.Workflow:
		if m := workflowPhraseRe.FindStringSubmatch(body); m != nil {
			a.WorkflowName = strings.TrimSpace(m[1])
		}
	default:
		a.UserName, a.UserID = parseRequester(body)
		if id := attrs["user"]; id != "" {
			a.UserID = id
		}
	}
	return a
}

func markerAttrs(body string) map[string]string {
	attrs := map[string]string{}
	m := prMarkerRe.FindStringSubmatch(body)
	if m == nil {
		return attrs
	}
	for _, kv := range strings.Fields(m[1]) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			attrs[k] = v
		}
	}
	return attrs
}

func parseRequester(body string) (name, id string) {
	m := requesterRe.FindStringSubmatch(body)
	if m == nil {
		return "", ""
	}
	who := strings.TrimSpace(m[1])
	if mm := slackMentionRe.FindStringSubmatch(who); mm != nil {
		id = mm[1]
		who = strings.TrimSpace(slackMentionRe.ReplaceAllString(who, ""))
	}
	if who == unresolvedUser || strings.HasPrefix(who, "<@") {
		who = ""
	}
	return who, id
}

// IsAutomatedPRBody reports whether a pull request body was written by arbetern.
func IsAutomatedPRBody(body string) bool {
	return strings.Contains(body, prMarkerPrefix)
}

// OriginKind is what stands behind a pull request on the console's Pull requests page.
type OriginKind string

const (
	OriginUser     OriginKind = "user"
	OriginWorkflow OriginKind = "workflow"
	OriginProject  OriginKind = "project"
)

// Origin is the person, workflow or project a pull request was opened for; Name is filled where the body carries it.
type Origin struct {
	Kind OriginKind `json:"kind"`
	ID   string     `json:"id,omitempty"`
	Name string     `json:"name,omitempty"`
}

// Origin returns who or what the pull request is listed under, or nil when the body names nobody.
func (a Attribution) Origin() *Origin {
	switch {
	case a.Source == origin.Project:
		return &Origin{Kind: OriginProject, ID: a.ProjectID}
	case a.Source == origin.Workflow:
		return &Origin{Kind: OriginWorkflow, ID: a.WorkflowID, Name: a.WorkflowName}
	case a.UserID != "" || a.UserName != "":
		return &Origin{Kind: OriginUser, ID: a.UserID, Name: a.UserName}
	}
	return nil
}

// markerValue drops a value that would not survive prMarkerRe and markerAttrs intact.
func markerValue(v string) string {
	v = strings.TrimSpace(v)
	if strings.ContainsFunc(v, func(r rune) bool { return r == '>' || unicode.IsSpace(r) }) {
		return ""
	}
	return v
}
