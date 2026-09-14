package github

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v85/github"
)

// prMarkerPrefix opens the HTML comment every arbetern-written PR body ends
// with; legacyPRMarkers are the attribution phrases older bodies carry.
const prMarkerPrefix = "<!-- arbetern"

var legacyPRMarkers = []string{"Automated via Slack by", "requested via Slack by"}

var (
	prMarkerRe     = regexp.MustCompile(`<!-- arbetern((?: [a-z]+=[^\s>]+)*) -->`)
	prRequesterRe  = regexp.MustCompile(`via Slack by (.+?)[._]*(?:\r?\n|$)`)
	slackMentionRe = regexp.MustCompile(`\s*\(?<@([A-Z0-9]+)(?:\|[^>]*)?>\)?`)
)

// PRMarker renders the body marker; empty attributes are left out.
func PRMarker(agent, source, user string) string {
	var b strings.Builder
	b.WriteString(prMarkerPrefix)
	for _, kv := range [][2]string{{"agent", agent}, {"source", source}, {"user", user}} {
		if v := strings.TrimSpace(kv[1]); v != "" {
			b.WriteString(" " + kv[0] + "=" + v)
		}
	}
	b.WriteString(" -->")
	return b.String()
}

// IsAutomatedPRBody reports whether a pull request body was written by arbetern.
func IsAutomatedPRBody(body string) bool {
	if strings.Contains(body, prMarkerPrefix) {
		return true
	}
	for _, m := range legacyPRMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// AutomatedPR is an open pull request arbetern authored, as listed by the
// console's Pull requests page.
type AutomatedPR struct {
	Repo        string    `json:"repo"`
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Draft       bool      `json:"draft"`
	Agent       string    `json:"agent,omitempty"`
	Source      string    `json:"source,omitempty"`
	RequestedBy string    `json:"requested_by,omitempty"`
	RequesterID string    `json:"requester_id,omitempty"`
	Comments    int       `json:"comments"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const maxAutomatedPRPages = 3

// ListOpenAutomatedPullRequests returns the open pull requests authored by the
// token's user whose body carries an arbetern marker: the ones ready for
// review first, then drafts, each group newest-updated first.
func (c *Client) ListOpenAutomatedPullRequests(ctx context.Context) ([]AutomatedPR, error) {
	login, err := c.GetAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf("type:pr is:open author:%s", login)
	opts := &gh.SearchOptions{
		Sort:        "updated",
		Order:       "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	out := []AutomatedPR{}
	for page := 0; page < maxAutomatedPRPages; page++ {
		results, resp, err := c.api.Search.Issues(ctx, q, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to search open pull requests: %w", err)
		}
		for _, issue := range results.Issues {
			body := issue.GetBody()
			if !IsAutomatedPRBody(body) {
				continue
			}
			owner, repo, number, err := ParsePRURL(issue.GetHTMLURL())
			if err != nil {
				continue
			}
			attrs := parsePRMarker(body)
			name, id := parsePRRequester(body)
			if attrs["user"] != "" {
				id = attrs["user"]
			}
			out = append(out, AutomatedPR{
				Repo:        owner + "/" + repo,
				Number:      number,
				Title:       issue.GetTitle(),
				URL:         issue.GetHTMLURL(),
				Draft:       issue.GetDraft(),
				Agent:       attrs["agent"],
				Source:      attrs["source"],
				RequestedBy: name,
				RequesterID: id,
				Comments:    issue.GetComments(),
				CreatedAt:   issue.GetCreatedAt().Time,
				UpdatedAt:   issue.GetUpdatedAt().Time,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	sort.SliceStable(out, func(i, j int) bool { return !out[i].Draft && out[j].Draft })
	return out, nil
}

func parsePRMarker(body string) map[string]string {
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

func parsePRRequester(body string) (name, id string) {
	m := prRequesterRe.FindStringSubmatch(body)
	if m == nil {
		return "", ""
	}
	who := strings.TrimSpace(m[1])
	if mm := slackMentionRe.FindStringSubmatch(who); mm != nil {
		id = mm[1]
		who = strings.TrimSpace(slackMentionRe.ReplaceAllString(who, ""))
	}
	return who, id
}
