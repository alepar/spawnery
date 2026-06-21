package cp

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// TestServerShutdownDrainsClaims verifies that Server.Shutdown blocks until an in-flight
// withClaim op releases its lease and that all claim metadata is nil after drain completes.
func TestServerShutdownDrainsClaims(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.claimTTL = 5 * time.Second

	seedActiveSpawn(t, s, "alice", "sp1")

	// block is closed by the test to release the in-flight withClaim.
	block := make(chan struct{})

	claimHeld := make(chan struct{}) // closed once the op holds the DB claim
	claimDone := make(chan error, 1) // final result of the withClaim goroutine

	go func() {
		err := s.withClaim(context.Background(), "sp1", func(cctx context.Context, leaseID string) error {
			close(claimHeld)
			<-block
			return nil
		})
		claimDone <- err
	}()

	// Wait until the claim is held before initiating shutdown.
	select {
	case <-claimHeld:
	case <-time.After(2 * time.Second):
		t.Fatal("withClaim goroutine never acquired the claim")
	}

	// Verify the DB row shows an active claim.
	sp, err := s.st.Spawns().Get(context.Background(), "sp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sp.ClaimHolder == nil {
		t.Fatal("expected claim to be held before shutdown")
	}

	sdCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- s.Shutdown(sdCtx)
	}()

	// Give Shutdown a moment to set claimDraining=true and start waiting — it must NOT return
	// while the op still holds the claim.
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned prematurely (err=%v) while claim is still held", err)
	default:
	}

	// Release the in-flight claim.
	close(block)

	// withClaim should complete.
	select {
	case err := <-claimDone:
		if err != nil {
			t.Fatalf("withClaim returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("withClaim goroutine did not finish after block released")
	}

	// Shutdown should now complete cleanly.
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not complete after claim released")
	}

	// Post-drain: claim metadata must be nil (no spawn claimed by exited CP).
	sp, err = s.st.Spawns().Get(context.Background(), "sp1")
	if err != nil {
		t.Fatalf("Get post-drain: %v", err)
	}
	if sp.ClaimHolder != nil || sp.ClaimLeaseID != nil || sp.ClaimDeadline != nil {
		t.Fatalf("claim metadata not cleared post-drain: holder=%v lease=%v deadline=%v",
			sp.ClaimHolder, sp.ClaimLeaseID, sp.ClaimDeadline)
	}
}

// TestServerShutdownRejectsNewClaimsDuringDrain verifies that once drain begins a fresh
// withClaim returns CodeUnavailable and does not acquire a DB lease.
func TestServerShutdownRejectsNewClaimsDuringDrain(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.claimTTL = 5 * time.Second

	seedActiveSpawn(t, s, "alice", "sp2")

	// Set claimDraining directly to simulate mid-drain state without needing a full Shutdown.
	s.claimMu.Lock()
	s.claimDraining = true
	s.claimMu.Unlock()

	err := s.withClaim(context.Background(), "sp2", func(cctx context.Context, leaseID string) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error when calling withClaim during drain, got nil")
	}
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("expected CodeUnavailable during drain, got %v", connect.CodeOf(err))
	}

	// Confirm no DB claim was taken.
	sp, gerr := s.st.Spawns().Get(context.Background(), "sp2")
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if sp.ClaimHolder != nil {
		t.Fatal("withClaim during drain must not acquire a DB claim")
	}
}

// TestServerShutdownClaimDrainBounded verifies that when a withClaim op never releases,
// Shutdown(ctx) returns ctx.Err() within the deadline and does not hang.
func TestServerShutdownClaimDrainBounded(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.claimTTL = 5 * time.Second

	seedActiveSpawn(t, s, "alice", "sp3")

	// Never unblocked — simulates a stalled in-flight op.
	never := make(chan struct{})

	claimHeld := make(chan struct{})
	go func() {
		_ = s.withClaim(context.Background(), "sp3", func(cctx context.Context, leaseID string) error {
			close(claimHeld)
			<-never // blocks forever
			return nil
		})
	}()

	select {
	case <-claimHeld:
	case <-time.After(2 * time.Second):
		t.Fatal("withClaim goroutine never acquired the claim")
	}

	// Short deadline — Shutdown must return within it.
	sdCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := s.Shutdown(sdCtx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded from bounded drain, got %v", err)
	}
	// Guard against the drain hanging well past the deadline.
	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %v, far exceeding the deadline — possible hang", elapsed)
	}

	// Unblock the stalled goroutine so the test goroutine can exit cleanly.
	close(never)
}
