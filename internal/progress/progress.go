// Package progress tracks the activity of one long-running agent turn so a
// caller can report it while the turn is still in flight.
package progress

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/safego"
)

// Snapshot is the state of a turn at one moment.
type Snapshot struct {
	StartedAt time.Time `json:"started_at"`
	ToolCalls int       `json:"tool_calls"`
	LastTool  string    `json:"last_tool,omitempty"`
}

// Elapsed is how long the turn has been running at now.
func (s Snapshot) Elapsed(now time.Time) time.Duration { return now.Sub(s.StartedAt) }

// Line renders the snapshot as one short status sentence.
func (s Snapshot) Line(now time.Time) string {
	msg := "Still working — " + FormatElapsed(s.Elapsed(now)) + " elapsed"
	if s.ToolCalls > 0 {
		msg += fmt.Sprintf(", %d tool calls (last: %s)", s.ToolCalls, s.LastTool)
	}
	return msg
}

// FormatElapsed renders d as whole seconds under a minute, whole minutes above.
func FormatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// Tracker records tool activity for one turn. All methods are nil-safe so a
// caller without a reporter can pass nil.
type Tracker struct {
	mu   sync.Mutex
	snap Snapshot
}

// NewTracker starts tracking a turn that begins now.
func NewTracker() *Tracker {
	return &Tracker{snap: Snapshot{StartedAt: time.Now().UTC()}}
}

// ToolCalled records that the turn invoked the named tool.
func (t *Tracker) ToolCalled(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.snap.ToolCalls++
	t.snap.LastTool = strings.ReplaceAll(name, "_", " ")
	t.mu.Unlock()
}

// Snapshot returns the current state.
func (t *Tracker) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snap
}

// Watch calls fn with a fresh snapshot after first, then every interval, until
// stop is closed.
func (t *Tracker) Watch(stop <-chan struct{}, first, every time.Duration, fn func(Snapshot)) {
	safego.Go("progress: watch", func() {
		timer := time.NewTimer(first)
		defer timer.Stop()
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		fn(t.Snapshot())
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				fn(t.Snapshot())
			}
		}
	})
}
