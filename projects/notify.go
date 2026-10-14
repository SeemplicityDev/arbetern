package projects

import (
	"fmt"
	"log"
	"strings"

	"github.com/justmike1/arbetern/internal/safego"
)

// Poster posts a message to a Slack channel.
type Poster interface {
	PostMessage(channelID, text string) (string, error)
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func slackText(s string, max int) string { return slackEscaper.Replace(oneLine(s, max)) }

func (r *Registry) notify(p *Project, text string) {
	channel := p.Notify.SlackChannel
	if channel == "" || r.deps.Slack == nil || text == "" {
		return
	}
	safego.Go("projects: notify "+p.ID, func() {
		if _, err := r.deps.Slack.PostMessage(channel, text); err != nil {
			log.Printf("[projects] %s: posting to Slack: %v", p.ID, err)
		}
	})
}

func subjectLabel(g Group) string {
	if len(g.Tags) == 0 {
		return slackText(groupLabel(g), 200)
	}
	values := make([]string, 0, len(g.Tags))
	for _, t := range g.Tags {
		if _, v, ok := strings.Cut(t, ":"); ok {
			t = v
		}
		values = append(values, t)
	}
	return slackText(strings.Join(values, " / ")+" ("+strings.ToUpper(g.Site)+")", 200)
}

func prLink(pr *PR) string {
	if pr == nil || !strings.HasPrefix(pr.URL, "https://") {
		return "its pull request"
	}
	return fmt.Sprintf("<%s|#%d>", pr.URL, pr.Number)
}

func noteOpened(t *Task, o Outcome) string {
	if o.Question != "" {
		return fmt.Sprintf(":large_yellow_circle: *%s* · %s needs a call: %s", subjectLabel(t.Group), prLink(t.PR), slackText(o.Question, maxQuestionChars))
	}
	return fmt.Sprintf(":large_green_circle: *%s* · opened %s: %s", subjectLabel(t.Group), prLink(t.PR), slackText(o.Title, 120))
}

func noteNoFix(t *Task, o Outcome) string {
	icon := ":white_circle:"
	if o.Status == OutcomeCannotFix {
		icon = ":red_circle:"
	}
	return fmt.Sprintf("%s *%s* · %s: %s", icon, subjectLabel(t.Group), strings.ReplaceAll(o.Status, "_", " "), slackText(o.Summary, 300))
}

func noteRound(t *Task, o Outcome) string {
	switch {
	case o.Question != "":
		return fmt.Sprintf(":large_yellow_circle: *%s* · %s needs a call: %s", subjectLabel(t.Group), prLink(t.PR), slackText(o.Question, maxQuestionChars))
	case o.Status == OutcomeCannotFix:
		return fmt.Sprintf(":red_circle: *%s* · %s is blocked: %s", subjectLabel(t.Group), prLink(t.PR), slackText(o.Summary, 300))
	}
	return ""
}

func noteReady(t *Task) string {
	return fmt.Sprintf(":eyes: *%s* · %s is ready for review: every check passes and every comment has an answer.", subjectLabel(t.Group), prLink(t.PR))
}

func noteExhausted(t *Task, rounds int) string {
	return fmt.Sprintf(":warning: *%s* · %s stopped after %s; over to a person.", subjectLabel(t.Group), prLink(t.PR), plural(rounds, "follow-up round"))
}

func noteClosed(t *Task, merged bool) string {
	if merged {
		return fmt.Sprintf(":white_check_mark: *%s* · %s merged.", subjectLabel(t.Group), prLink(t.PR))
	}
	return fmt.Sprintf(":x: *%s* · %s was closed without merging.", subjectLabel(t.Group), prLink(t.PR))
}

func notePaused(p *Project, reason string) string {
	return fmt.Sprintf(":rotating_light: Project *%s* (%s) paused itself: %s", slackText(p.Name, 80), p.ID, slackText(reason, 500))
}
