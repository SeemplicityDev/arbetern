// Package safego runs work under a panic guard.
//
// An unrecovered panic outside an HTTP handler kills the whole process (Go exit
// status 2). net/http recovers panics in handlers, but this app does its real
// work elsewhere — Slack commands, workflow ticks and dashboard refreshes all
// run on bare goroutines — so one bad tool argument can restart the pod.
//
// Granularity matters: guarding a dispatch goroutine at its boundary is right,
// but a ticker or event loop must guard the per-iteration work with Run, or
// recovering still ends the loop and leaves the process up but silently idle.
package safego

import (
	"log"
	"runtime/debug"
	"time"
)

// Go runs fn in a new goroutine under Recover, for one-shot dispatch work.
func Go(name string, fn func()) {
	go Run(name, fn)
}

// Run invokes fn on the calling goroutine under Recover. Use it inside a loop so
// a panicking iteration is logged and skipped while the loop keeps running.
func Run(name string, fn func()) {
	defer Recover(name)
	fn()
}

// Every runs fn on each tick until stop is closed, then once more, and closes
// the returned channel when that final run is done. Each run is guarded
// separately, so a panicking iteration is logged and skipped instead of ending
// the loop and leaving the process up but silently idle.
//
// The final run is why this exists rather than a bare ticker: the callers are
// buffers that must be drained on the way out, and a shutdown that skips that
// drain loses whatever they were holding.
func Every(name string, interval time.Duration, stop <-chan struct{}, fn func()) <-chan struct{} {
	done := make(chan struct{})
	run := func() { Run(name, fn) }
	Go(name+" loop", func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				run()
				return
			case <-t.C:
				run()
			}
		}
	})
	return done
}

// Recover is the deferred guard, for goroutines that must control defer order —
// a `defer wg.Done()` or `defer close(done)` that still has to fire. Defers run
// LIFO, so declare this one last and it runs first, absorbing the panic before
// the others.
func Recover(name string) {
	if v := recover(); v != nil {
		log.Printf("[panic] recovered in %s: %v\n%s", name, v, debug.Stack())
	}
}
