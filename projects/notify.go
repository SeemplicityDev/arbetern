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
	return fmt.Sprintf("<%s|PR #%d>", pr.URL, pr.Number)
}

// headline names the task the way its session put it for the channel, else by its series or error group.
func headline(t *Task) string {
	if t.Outcome != nil && t.Outcome.Headline != "" {
		return slackText(t.Outcome.Headline, maxHeadlineChars)
	}
	return "*" + subjectLabel(t.Group) + "*"
}

// links are the task's pull request and ticket, as the channel shows them.
func links(t *Task) string {
	parts := []string{prLink(t.PR)}
	if t.Ticket != "" {
		if strings.HasPrefix(t.TicketURL, "https://") {
			parts = append(parts, fmt.Sprintf("<%s|%s>", t.TicketURL, slackText(t.Ticket, 40)))
		} else {
			parts = append(parts, slackText(t.Ticket, 40))
		}
	}
	return strings.Join(parts, " · ")
}

func belowThreshold(t *Task) string {
	if t.Group.Reason == reasonFill {
		return " _(below threshold)_"
	}
	return ""
}

func noteOpened(t *Task, o Outcome) string {
	if o.Question != "" {
		return fmt.Sprintf(":large_yellow_circle: %s — %s — need a call: %s%s", headline(t), links(t), slackText(o.Question, maxQuestionChars), belowThreshold(t))
	}
	return fmt.Sprintf(":large_green_circle: %s — %s%s", headline(t), links(t), belowThreshold(t))
}

func noteNoFix(t *Task, o Outcome) string {
	if o.Status == OutcomeCannotFix {
		return fmt.Sprintf(":red_circle: %s — need your review: %s%s", headline(t), slackText(o.Summary, 300), belowThreshold(t))
	}
	return fmt.Sprintf(":white_circle: %s — %s: %s%s", headline(t), strings.ReplaceAll(o.Status, "_", " "), slackText(o.Summary, 300), belowThreshold(t))
}

func noteRound(t *Task, o Outcome) string {
	switch {
	case o.Question != "":
		return fmt.Sprintf(":large_yellow_circle: %s — %s — need a call: %s", headline(t), links(t), slackText(o.Question, maxQuestionChars))
	case o.Status == OutcomeCannotFix:
		return fmt.Sprintf(":red_circle: %s — %s — need your review: %s", headline(t), links(t), slackText(o.Summary, 300))
	}
	return ""
}

func noteReady(t *Task) string {
	return fmt.Sprintf(":eyes: %s — %s is ready for review: every check passes and every comment has an answer.", headline(t), links(t))
}

func noteExhausted(t *Task, rounds int) string {
	return fmt.Sprintf(":warning: %s — %s stopped after %s; over to a person.", headline(t), links(t), plural(rounds, "follow-up round"))
}

func noteClosed(t *Task, merged bool) string {
	if merged {
		return fmt.Sprintf(":white_check_mark: %s — %s merged.", headline(t), links(t))
	}
	return fmt.Sprintf(":x: %s — %s was closed without merging.", headline(t), links(t))
}

func notePaused(p *Project, reason string) string {
	return fmt.Sprintf(":rotating_light: Project *%s* (%s) paused itself: %s", slackText(p.Name, 80), p.ID, slackText(reason, 500))
}
