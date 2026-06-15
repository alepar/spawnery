package journal

import (
	"context"
	"sync"
)

// snapshotFn takes one snapshot of a mount, returning its manifest id.
type snapshotFn func(ctx context.Context) (ManifestID, error)

// serialQueue serializes snapshots for a single mount and provides the suspend
// barrier (design §2, roast M17): snapshots run one at a time; multiple requests
// while one is in flight coalesce into a single follow-up; Suspend stops
// accepting new work, drains the in-flight snapshot, then takes the final
// snapshot exclusively.
//
// One background goroutine at most is live per queue (the runner), so
// one-at-a-time execution is structural, not lock-timing-dependent. Tests drive
// it deterministically by gating the action on channels.
type serialQueue struct {
	action snapshotFn // takes one snapshot; supplied by the Manager

	mu        sync.Mutex
	cond      *sync.Cond
	running   bool // a runner goroutine is executing/looping
	pending   bool // a snapshot was requested while running (coalesced)
	suspended bool // Suspend called; no new work accepted
	bgCtx     context.Context
}

func newSerialQueue(bg context.Context, action snapshotFn) *serialQueue {
	q := &serialQueue{action: action, bgCtx: bg}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Request schedules a snapshot. If one is already running, the request coalesces
// (a single follow-up snapshot will run after the current one). No-op once
// suspended.
func (q *serialQueue) Request() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.suspended {
		return
	}
	q.pending = true
	if !q.running {
		q.running = true
		go q.run()
	}
}

// run is the single runner goroutine: it drains coalesced requests one at a
// time until none remain (or suspend intervenes), then exits.
func (q *serialQueue) run() {
	for {
		q.mu.Lock()
		if q.suspended || !q.pending {
			q.running = false
			q.cond.Broadcast() // wake any Suspend waiting for the drain
			q.mu.Unlock()
			return
		}
		q.pending = false
		q.mu.Unlock()

		// Best-effort: a failed periodic snapshot is non-fatal; the next request
		// (or the suspend final) retries. Errors surface on the suspend path.
		_, _ = q.action(q.bgCtx)
	}
}

// Suspend implements the barrier: refuse new requests, wait for any in-flight
// snapshot to drain, then run final exclusively and return its manifest id.
// final is typically the same action as the queue's, invoked with the
// suspend ctx so the caller controls its deadline.
func (q *serialQueue) Suspend(ctx context.Context, final snapshotFn) (ManifestID, error) {
	q.mu.Lock()
	q.suspended = true
	q.pending = false // cancel any coalesced (debounced) pending request
	for q.running {
		q.cond.Wait() // drain the in-flight snapshot
	}
	q.mu.Unlock()

	// Runner has exited; we hold exclusive access to the mount.
	return final(ctx)
}

// WarmSnapshot drains the queue, runs one immediate snapshot exclusively, and
// leaves the queue open for later Request calls.
func (q *serialQueue) WarmSnapshot(ctx context.Context, warm snapshotFn) (ManifestID, error) {
	q.mu.Lock()
	for q.running {
		q.cond.Wait()
	}
	q.running = true
	q.mu.Unlock()

	id, err := warm(ctx)

	q.mu.Lock()
	q.running = false
	if q.pending && !q.suspended {
		q.running = true
		go q.run()
	}
	q.cond.Broadcast()
	q.mu.Unlock()
	return id, err
}

// IsSuspended reports whether the queue has been suspended.
func (q *serialQueue) IsSuspended() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.suspended
}
