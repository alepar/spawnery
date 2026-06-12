package cp

// pendingintent.go implements the CP-side two-phase sign-after-resolve mechanism [AC1].
// The lifecycle handlers (Create/Resume/Recreate/Migrate) register a PendingIntent with the
// committed tuple BEFORE calling Provision. The client polls GetPendingIntent, validates the
// tuple against its own pended record, signs an IntentBody, and calls SubmitIntent to deliver
// the AuthEnvelope. Provision blocks on Await until the envelope arrives (or TTL expires).
//
// Design invariants:
//  - At most one pending entry per spawnID at a time (the per-spawn lock the lifecycle handlers
//    hold prevents concurrent entries for the same ID).
//  - ownerID guard: only the spawn owner may call SubmitIntent; a foreign Submit is refused.
//  - TTL: if the client never signs within the window the provision path gets ctx-cancel/error.
//  - The channel is buffered (cap 1) so Submit never blocks regardless of the Await timing.

import (
	"context"
	"fmt"
	"sync"
	"time"

	cpv1 "spawnery/gen/cp/v1"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/intent"
)

// defaultIntentTTL is how long a lifecycle handler waits for the client to SubmitIntent before
// failing. It is slightly larger than the intent freshness window (FreshnessWindow + SkewBudget)
// so the node still accepts an intent submitted at the last moment.
const defaultIntentTTL = intent.FreshnessWindow + intent.SkewBudget + 5*time.Second

// pendingIntentRegistry is the per-CP registry of in-flight pending intents.
// One entry exists per spawnID; it is registered before Provision and cleaned up after.
type pendingIntentRegistry struct {
	mu      sync.Mutex
	entries map[string]*intentEntry
	ttl     time.Duration // injectable for tests
}

type intentEntry struct {
	ownerID string
	pending *cpv1.PendingIntent
	ch      chan *authv1.AuthEnvelope // buffered cap 1: Submit is non-blocking
}

func newPendingIntentRegistry() *pendingIntentRegistry {
	return &pendingIntentRegistry{
		entries: map[string]*intentEntry{},
		ttl:     defaultIntentTTL,
	}
}

// register installs a pending intent for a spawn. Returns the channel to Await on.
// Panics (programmer error) if an entry already exists for spawnID — the per-spawn lock
// the caller holds means this should never happen in practice.
func (r *pendingIntentRegistry) register(spawnID, ownerID string, pi *cpv1.PendingIntent) chan *authv1.AuthEnvelope {
	ch := make(chan *authv1.AuthEnvelope, 1)
	r.mu.Lock()
	r.entries[spawnID] = &intentEntry{ownerID: ownerID, pending: pi, ch: ch}
	r.mu.Unlock()
	return ch
}

// await blocks until a SignedIntent arrives, the ctx is cancelled, or the TTL fires.
// Returns the AuthEnvelope on success. On timeout or cancel the caller should abort Provision.
func (r *pendingIntentRegistry) await(ctx context.Context, ch chan *authv1.AuthEnvelope) (*authv1.AuthEnvelope, error) {
	ttl := r.ttl
	if ttl <= 0 {
		ttl = defaultIntentTTL
	}
	select {
	case env := <-ch:
		return env, nil
	case <-time.After(ttl):
		return nil, fmt.Errorf("timed out waiting for client SignedIntent (TTL %s)", ttl)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// get returns the PendingIntent for a spawn (for GetPendingIntent RPC polls). If no entry
// exists, returns (nil, false). The second return is true iff the entry is present (ready).
func (r *pendingIntentRegistry) get(spawnID string) (*cpv1.PendingIntent, bool) {
	r.mu.Lock()
	e, ok := r.entries[spawnID]
	r.mu.Unlock()
	if !ok {
		return nil, false
	}
	return e.pending, true
}

// submit validates ownership and routes the AuthEnvelope into the channel.
// Returns an error if: no pending entry, wrong owner, or channel already consumed (double-submit).
func (r *pendingIntentRegistry) submit(spawnID, ownerID string, env *authv1.AuthEnvelope) error {
	r.mu.Lock()
	e, ok := r.entries[spawnID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending intent for spawn %q", spawnID)
	}
	if e.ownerID != ownerID {
		return fmt.Errorf("permission denied: submitter %q is not the owner of spawn %q", ownerID, spawnID)
	}
	select {
	case e.ch <- env:
		return nil
	default:
		return fmt.Errorf("intent already submitted for spawn %q", spawnID)
	}
}

// cleanup removes the entry for a spawn after provision completes (success or failure).
func (r *pendingIntentRegistry) cleanup(spawnID string) {
	r.mu.Lock()
	delete(r.entries, spawnID)
	r.mu.Unlock()
}
