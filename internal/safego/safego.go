// Package safego provides panic-recovery wrappers for long-running goroutines.
//
// Two primitives cover the two policies:
//
//   - Run — RESTART primitive: run fn synchronously under a deferred recover(). A panic is
//     logged and Run returns normally. Intended for per-tick work bodies inside an outer
//     loop; the loop continues after the recovered tick.
//
//   - Go — TERMINATE primitive: launch fn in a new goroutine via Go(name, fn) { go Run(name, fn) }.
//     A panic in fn is recovered and logged, but fn's goroutine exits (it ran once). Intended
//     for detached goroutines where the correct response to a panic is "stop that goroutine,
//     keep the process alive".
package safego

import (
	"log"
	"runtime/debug"
)

// Run executes fn under a deferred recover(). If fn panics, the panic value and a stack
// trace are logged as "goroutine-panic [name]: <value>\n<stack>" and Run returns normally.
// Use Run to wrap the work body of a loop so a panicking iteration is recovered and the
// loop can continue (RESTART policy).
func Run(name string, fn func()) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("goroutine-panic [%s]: %v\n%s", name, rec, debug.Stack())
		}
	}()
	fn()
}

// Go launches fn in a new goroutine under a deferred recover(). A panic in fn is recovered
// and logged (see Run), but fn's goroutine exits after the panic (TERMINATE policy). Use Go
// for detached goroutines where the correct response to a panic is to stop that goroutine
// while keeping the process alive.
func Go(name string, fn func()) {
	go Run(name, fn)
}
