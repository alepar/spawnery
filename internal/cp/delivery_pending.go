package cp

import (
	"sync"
	"time"
)

// deliveryPendingDeadline is how long the CP waits for the owner to deliver the
// journal key to the target node after a migration before the "pending" flag is
// auto-cleared. At that point the spawn is already active on the target; the
// flag is informational (prompts the web UI to show a delivery step) and its
// expiry simply stops the prompt — it does NOT revert the spawn. A follow-up
// slice (sp-e642) may implement a revert-on-timeout state machine.
const deliveryPendingDeadline = 5 * time.Minute

// deliveryPendingEntry records that a spawn is waiting for a journal-key delivery.
type deliveryPendingEntry struct {
	deadline time.Time
}

// deliveryPendingTracker is a thread-safe in-memory set of spawns waiting for
// an owner-sealed journal-key delivery after a cross-node migration.
type deliveryPendingTracker struct {
	mu  sync.Mutex
	m   map[string]deliveryPendingEntry
	now func() time.Time // injectable for tests; nil uses time.Now
}

func newDeliveryPendingTracker() *deliveryPendingTracker {
	return &deliveryPendingTracker{m: map[string]deliveryPendingEntry{}}
}

func (t *deliveryPendingTracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// mark records spawnID as delivery-pending with a fixed deadline. Idempotent:
// a second mark refreshes the deadline.
func (t *deliveryPendingTracker) mark(spawnID string) {
	t.mu.Lock()
	t.m[spawnID] = deliveryPendingEntry{deadline: t.clock().Add(deliveryPendingDeadline)}
	t.mu.Unlock()
}

// clear removes the pending entry for spawnID (delivery completed or migrated back).
func (t *deliveryPendingTracker) clear(spawnID string) {
	t.mu.Lock()
	delete(t.m, spawnID)
	t.mu.Unlock()
}

// isPending reports whether spawnID is waiting for a journal-key delivery and
// the deadline has not yet passed.
func (t *deliveryPendingTracker) isPending(spawnID string) bool {
	t.mu.Lock()
	e, ok := t.m[spawnID]
	t.mu.Unlock()
	return ok && t.clock().Before(e.deadline)
}
