package commands

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/progress"
)

const (
	progressFirstAfter = 45 * time.Second
	progressEvery      = time.Minute
)

// slackProgress mirrors a turn's progress into one thread reply that is edited
// in place and removed when the answer lands.
type slackProgress struct {
	*progress.Tracker
	slack     SlackClient
	channelID string
	threadTS  string
	stop      chan struct{}
	once      sync.Once

	postMu   sync.Mutex
	ts       string
	finished bool
}

func startSlackProgress(slack SlackClient, channelID, threadTS string) *slackProgress {
	if slack == nil || channelID == "" || threadTS == "" {
		return nil
	}
	p := &slackProgress{Tracker: progress.NewTracker(), slack: slack, channelID: channelID, threadTS: threadTS, stop: make(chan struct{})}
	p.Watch(p.stop, progressFirstAfter, progressEvery, p.publish)
	return p
}

func (p *slackProgress) tracker() *progress.Tracker {
	if p == nil {
		return nil
	}
	return p.Tracker
}

func (p *slackProgress) publish(s progress.Snapshot) {
	p.postMu.Lock()
	defer p.postMu.Unlock()
	if p.finished {
		return
	}
	text := "_:hourglass_flowing_sand: " + s.Line(time.Now()) + "_"
	if s.Plan != "" {
		text += "\n" + quoteLines(s.Plan)
	}
	if p.ts == "" {
		ts, err := p.slack.PostMessageInThread(p.channelID, p.threadTS, text)
		if err != nil {
			log.Printf("[progress] post failed channel=%s thread=%s: %v", p.channelID, p.threadTS, err)
			return
		}
		p.ts = ts
		return
	}
	if err := p.slack.UpdateMessage(p.channelID, p.ts, text); err != nil {
		log.Printf("[progress] update failed channel=%s thread=%s: %v", p.channelID, p.threadTS, err)
	}
}

// quoteLines renders the plan as a Slack blockquote so it reads as context
// under the status line rather than as a second message.
func quoteLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = "> " + strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}

func (p *slackProgress) done() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		close(p.stop)
		p.postMu.Lock()
		defer p.postMu.Unlock()
		p.finished = true
		if p.ts == "" {
			return
		}
		if err := p.slack.DeleteMessage(p.channelID, p.ts); err != nil {
			log.Printf("[progress] delete failed channel=%s thread=%s: %v", p.channelID, p.threadTS, err)
		}
	})
}
