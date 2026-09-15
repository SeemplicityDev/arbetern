package llm

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// CallStats describes one completed provider round-trip, retries folded in.
// It carries no prompt, no reply and no requester — only what is needed to
// watch how the backend is performing.
type CallStats struct {
	Model       string
	Backend     string
	Latency     time.Duration
	Attempts    int
	RateLimited bool
	Failed      bool
}

// Observer receives every completion the client makes.
type Observer interface{ ObserveCall(CallStats) }

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(CallStats)

// ObserveCall implements Observer.
func (f ObserverFunc) ObserveCall(s CallStats) { f(s) }

var (
	observerMu sync.RWMutex
	observer   Observer
)

// SetObserver installs the process-wide call observer. Clients are cloned per
// agent and per model override, so the hook is held here rather than on each
// one. A nil observer disables reporting.
func SetObserver(o Observer) {
	observerMu.Lock()
	observer = o
	observerMu.Unlock()
}

func currentObserver() Observer {
	observerMu.RLock()
	defer observerMu.RUnlock()
	return observer
}

// attempts counts the HTTP attempts one completion spent, including the
// retries doPostWithRetry makes several layers below the caller.
type attempts struct {
	mu      sync.Mutex
	n       int
	limited bool
}

type attemptsKey struct{}

func withAttempts(ctx context.Context) (context.Context, *attempts) {
	a := &attempts{}
	return context.WithValue(ctx, attemptsKey{}, a), a
}

func noteAttempt(ctx context.Context, status int) {
	a, _ := ctx.Value(attemptsKey{}).(*attempts)
	if a == nil {
		return
	}
	a.mu.Lock()
	a.n++
	if status == http.StatusTooManyRequests {
		a.limited = true
	}
	a.mu.Unlock()
}

func (a *attempts) read() (int, bool) {
	if a == nil {
		return 1, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n, a.limited
}

// backendName labels which provider path a completion took.
func (c *Client) backendName() string {
	switch {
	case c.useBedrock():
		return "bedrock"
	case c.isResponsesModel():
		return "azure-responses"
	case c.useAzure() && isAnthropicModel(c.model):
		return "azure-anthropic"
	case c.useAzure():
		return "azure"
	default:
		return "github-models"
	}
}
