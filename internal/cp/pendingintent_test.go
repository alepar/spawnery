package cp

// pendingintent_test.go: unit tests for pendingIntentRegistry covering the branches not
// exercised by intent_threading_test.go: TTL expiry and the owner-mismatch guard [AC1].

import (
	"context"
	"testing"
	"time"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
)

// testEnv returns a minimal non-nil AuthEnvelope for submit calls.
func testEnv() *authv1.AuthEnvelope {
	return &authv1.AuthEnvelope{AccessToken: "tok", Intent: &authv1.SignedIntent{Domain: "d"}}
}

// testPI returns a minimal PendingIntent.
func testPI(spawnID string) *cpv1.PendingIntent {
	return &cpv1.PendingIntent{SpawnId: spawnID, Generation: 1}
}

// TestPendingIntentTTLExpiry: await must return an error when no SubmitIntent arrives within
// the registry TTL. The lifecycle handler uses this error to abort provision and set spawn error.
func TestPendingIntentTTLExpiry(t *testing.T) {
	r := newPendingIntentRegistry()
	r.ttl = 20 * time.Millisecond // short TTL so the test completes quickly

	ch := r.register("spawn-ttl", "alice", testPI("spawn-ttl"))
	_, err := r.await(context.Background(), ch)
	if err == nil {
		t.Fatal("await must return error on TTL expiry; got nil")
	}
}

// TestPendingIntentContextCancel: await must return when the context is cancelled (e.g., client
// disconnect mid-provision), not wait for TTL.
func TestPendingIntentContextCancel(t *testing.T) {
	r := newPendingIntentRegistry()
	r.ttl = 10 * time.Second // long TTL; we cancel the ctx instead

	ch := r.register("spawn-cancel", "alice", testPI("spawn-cancel"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	_, err := r.await(ctx, ch)
	if err == nil {
		t.Fatal("await must return error on context cancel; got nil")
	}
}

// TestPendingIntentOwnerMismatch: submit must refuse an envelope from a non-owner.
// This guards against a compromised CP routing a foreign SubmitIntent to steal a provision slot.
func TestPendingIntentOwnerMismatch(t *testing.T) {
	r := newPendingIntentRegistry()
	r.register("spawn-owner", "alice", testPI("spawn-owner"))

	// Bob tries to submit for Alice's spawn.
	err := r.submit("spawn-owner", "bob", testEnv())
	if err == nil {
		t.Fatal("submit must return error for wrong owner; got nil")
	}
}

// TestPendingIntentDoubleSubmit: a second submit for the same spawnID must be refused.
// The buffered channel (cap 1) enforces exactly-once delivery without blocking.
func TestPendingIntentDoubleSubmit(t *testing.T) {
	r := newPendingIntentRegistry()
	r.register("spawn-double", "alice", testPI("spawn-double"))

	if err := r.submit("spawn-double", "alice", testEnv()); err != nil {
		t.Fatalf("first submit should succeed; got: %v", err)
	}
	if err := r.submit("spawn-double", "alice", testEnv()); err == nil {
		t.Fatal("second submit must return error (already submitted); got nil")
	}
}

// TestPendingIntentGetNotReady: get returns (nil, false) when no entry exists.
func TestPendingIntentGetNotReady(t *testing.T) {
	r := newPendingIntentRegistry()
	pi, ready := r.get("no-such-spawn")
	if ready || pi != nil {
		t.Fatal("get for unknown spawn must return (nil, false)")
	}
}

// TestPendingIntentGetReady: get returns the PendingIntent and true after register.
func TestPendingIntentGetReady(t *testing.T) {
	r := newPendingIntentRegistry()
	want := testPI("spawn-ready")
	r.register("spawn-ready", "alice", want)

	pi, ready := r.get("spawn-ready")
	if !ready {
		t.Fatal("get must return ready=true after register")
	}
	if pi.GetSpawnId() != "spawn-ready" {
		t.Fatalf("get returned wrong PI: %+v", pi)
	}
}

// TestPendingIntentSubmitNoEntry: submit for a non-registered spawn must return an error.
func TestPendingIntentSubmitNoEntry(t *testing.T) {
	r := newPendingIntentRegistry()
	if err := r.submit("ghost-spawn", "alice", testEnv()); err == nil {
		t.Fatal("submit for unknown spawn must return error; got nil")
	}
}

// TestPendingIntentCleanup: after cleanup, get returns (nil, false) and submit returns an error.
func TestPendingIntentCleanup(t *testing.T) {
	r := newPendingIntentRegistry()
	r.register("spawn-clean", "alice", testPI("spawn-clean"))
	r.cleanup("spawn-clean")

	if _, ready := r.get("spawn-clean"); ready {
		t.Fatal("get after cleanup must return ready=false")
	}
	if err := r.submit("spawn-clean", "alice", testEnv()); err == nil {
		t.Fatal("submit after cleanup must return error; got nil")
	}
}

// TestPendingIntentAwaitSuccess: await returns the envelope delivered by submit.
func TestPendingIntentAwaitSuccess(t *testing.T) {
	r := newPendingIntentRegistry()
	r.ttl = 5 * time.Second

	ch := r.register("spawn-ok", "alice", testPI("spawn-ok"))
	env := testEnv()
	// Submit in background so await and submit race as they would in production.
	go func() { _ = r.submit("spawn-ok", "alice", env) }()

	got, err := r.await(context.Background(), ch)
	if err != nil {
		t.Fatalf("await must succeed; got: %v", err)
	}
	if got.GetAccessToken() != env.GetAccessToken() {
		t.Fatalf("await returned wrong envelope: %+v", got)
	}
}
