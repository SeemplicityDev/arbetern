package commands

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/safego"
)

const (
	progressFirstAfter = 45 * time.Second
	progressEvery      = time.Minute
)

// progressReporter keeps a single "still working" reply in the thread while a
// long turn runs, edits it in place, and removes it once the answer lands.
type progressReporter struct {
	slack     SlackClient
	channelID string
	threadTS  string
	started   time.Time
	stop      chan struct{}
	once      sync.Once

	postMu   sync.Mutex
	ts       string
	finished bool

	mu        sync.Mutex
	toolCalls int
	lastTool  string
}

func startProgressReporter(slack SlackClient, channelID, threadTS string) *progressReporter {
	if slack == nil || channelID == "" || threadTS == "" {
		return nil
	}
	p := &progressReporter{slack: slack, channelID: channelID, threadTS: threadTS, started: time.Now(), stop: make(chan struct{})}
	safego.Go("slack: progress reporter", p.run)
	return p
}

func (p *progressReporter) run() {
	first := time.NewTimer(progressFirstAfter)
	defer first.Stop()
	select {
	case <-p.stop:
		return
	case <-first.C:
	}
	p.publish()
	t := time.NewTicker(progressEvery)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.publish()
		}
	}
}

func (p *progressReporter) toolCalled(name string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.toolCalls++
	p.lastTool = strings.ReplaceAll(name, "_", " ")
	p.mu.Unlock()
}

func (p *progressReporter) text() string {
	p.mu.Lock()
	calls, last := p.toolCalls, p.lastTool
	p.mu.Unlock()
	elapsed := time.Since(p.started)
	var age string
	if elapsed < time.Minute {
		age = fmt.Sprintf("%ds", int(elapsed.Seconds()))
	} else {
		age = fmt.Sprintf("%dm", int(elapsed.Minutes()))
	}
	msg := ":hourglass_flowing_sand: Still working — " + age + " elapsed"
	if calls > 0 {
		msg += fmt.Sprintf(", %d tool calls (last: %s)", calls, last)
	}
	return "_" + msg + "_"
}

func (p *progressReporter) publish() {
	p.postMu.Lock()
	defer p.postMu.Unlock()
	if p.finished {
		return
	}
	text := p.text()
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

// done stops the updates and deletes the progress message, if one was posted.
func (p *progressReporter) done() {
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
