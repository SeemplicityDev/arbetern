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

// Recover is the deferred guard, for goroutines that must control defer order —
// a `defer wg.Done()` or `defer close(done)` that still has to fire. Defers run
// LIFO, so declare this one last and it runs first, absorbing the panic before
// the others.
func Recover(name string) {
	if v := recover(); v != nil {
		log.Printf("[panic] recovered in %s: %v\n%s", name, v, debug.Stack())
	}
}
