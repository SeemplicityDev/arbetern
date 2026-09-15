package llm

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"
)

// A breaker stops calling a dependency that is failing, so one sick endpoint
// costs a bounded amount of wall clock instead of its full timeout on every
// call. It matters most for the compression proxy: compression sits in front of
// every LLM round, so a hung sidecar multiplies its timeout by the round count
// and a single tool loop can spend its entire budget waiting for a service
// whose result is optional.
//
// Failures are weighted by what they cost to discover. A refused connection or
// an error status comes back immediately, so a couple of them are tolerated; a
// timeout has already burned the whole budget, so one is enough to open.
const (
	breakerFailureThreshold = 3
	breakerCooldownMin      = 30 * time.Second
	breakerCooldownMax      = 10 * time.Minute
)

type breaker struct {
	// key identifies the endpoint and may carry its URL; label is what the
	// console shows. They are separate so a configured internal address stays
	// in this process and out of an API response.
	key   string
	label string

	mu        sync.Mutex
	failures  int
	cooldown  time.Duration
	openUntil time.Time
	probing   bool
}

var (
	breakersMu sync.Mutex
	breakers   = map[string]*breaker{}
)

// breakerFor returns the shared breaker for an endpoint. Clients are cloned per
// agent and per model override but all point at the same sidecar, so the health
// of that sidecar is tracked once rather than per clone.
func breakerFor(key, label string) *breaker {
	breakersMu.Lock()
	defer breakersMu.Unlock()
	b := breakers[key]
	if b == nil {
		b = &breaker{key: key, label: label, cooldown: breakerCooldownMin}
		breakers[key] = b
	}
	return b
}

// allow reports whether a call may go out. While open it lets exactly one
// probe through per cooldown, so recovery is detected without paying the
// timeout on every concurrent caller.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if time.Now().Before(b.openUntil) || b.probing {
		return false
	}
	b.probing = true
	return true
}

func (b *breaker) ok() {
	b.mu.Lock()
	defer b.mu.Unlock()
	reopened := !b.openUntil.IsZero()
	b.failures, b.probing, b.cooldown = 0, false, breakerCooldownMin
	b.openUntil = time.Time{}
	if reopened {
		log.Printf("[llm] %s recovered, resuming", b.label)
	}
}

// fail records a failed call and opens the breaker once the endpoint has cost
// enough. A deadline hit opens it on its own.
func (b *breaker) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if errors.Is(err, context.DeadlineExceeded) {
		b.failures = breakerFailureThreshold
	} else {
		b.failures++
	}
	if b.failures < breakerFailureThreshold {
		b.probing = false
		return
	}
	if b.probing {
		// The probe failed: wait longer before the next one.
		b.cooldown *= 2
		if b.cooldown > breakerCooldownMax {
			b.cooldown = breakerCooldownMax
		}
	}
	b.probing = false
	b.openUntil = time.Now().Add(b.cooldown)
	log.Printf("[llm] %s unhealthy, skipping it for %s: %v", b.label, b.cooldown, err)
}

// ErrDependencyDown is returned instead of calling an endpoint whose breaker is
// open. Callers treat it like any other failure of that endpoint.
var ErrDependencyDown = errors.New("dependency is unhealthy and is being skipped")

// Health describes one endpoint the client has stopped trusting. It names the
// service, never its address: this is served over the console API, and where a
// sidecar or model actually lives is not something that response should carry.
type Health struct {
	Service  string `json:"service"`
	Healthy  bool   `json:"healthy"`
	Failures int    `json:"failures,omitempty"`
	RetryIn  string `json:"retry_in,omitempty"`
}

// Dependencies reports the state of every optional endpoint the client guards,
// so an operator can see that compression is off rather than infer it from a
// token count that quietly stopped shrinking.
func Dependencies() []Health {
	breakersMu.Lock()
	all := make([]*breaker, 0, len(breakers))
	for _, b := range breakers {
		all = append(all, b)
	}
	breakersMu.Unlock()

	out := make([]Health, 0, len(all))
	for _, b := range all {
		b.mu.Lock()
		h := Health{Service: b.label, Healthy: b.openUntil.IsZero(), Failures: b.failures}
		if !h.Healthy {
			if d := time.Until(b.openUntil); d > 0 {
				h.RetryIn = d.Round(time.Second).String()
			} else {
				h.RetryIn = "now"
			}
		}
		b.mu.Unlock()
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}
