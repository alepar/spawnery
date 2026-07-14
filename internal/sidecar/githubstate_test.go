package sidecar

import (
	"context"
	"testing"
	"time"
)

func TestGitHubState_SetAndToken(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	s := NewGitHubState()

	if tok, _ := s.Token(); tok != "" {
		t.Fatalf("fresh state Token() = %q, want empty", tok)
	}
	if _, err := s.LeafFor("github.com"); err == nil {
		t.Fatalf("LeafFor on a fresh state: want error, got nil")
	}

	if err := s.Set(certPEM, keyPEM, "ghs_one", 1234); err != nil {
		t.Fatalf("Set: %v", err)
	}
	tok, exp := s.Token()
	if tok != "ghs_one" || exp != 1234 {
		t.Fatalf("Token() = (%q,%d), want (ghs_one,1234)", tok, exp)
	}
	leaf, err := s.LeafFor("github.com")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	if leaf.Leaf.Subject.CommonName != "github.com" {
		t.Fatalf("leaf CN = %q, want github.com", leaf.Leaf.Subject.CommonName)
	}

	// Idempotent replacement: same CA PEMs, new token.
	if err := s.Set(certPEM, keyPEM, "ghs_two", 5678); err != nil {
		t.Fatalf("Set (replace): %v", err)
	}
	tok, exp = s.Token()
	if tok != "ghs_two" || exp != 5678 {
		t.Fatalf("Token() after replace = (%q,%d), want (ghs_two,5678)", tok, exp)
	}

	// Re-pushing byte-identical CA PEMs must NOT re-parse the CA: the leaf cache survives.
	leaf2, err := s.LeafFor("github.com")
	if err != nil {
		t.Fatalf("LeafFor after replace: %v", err)
	}
	if leaf2 != leaf {
		t.Fatalf("identical CA re-push discarded the leaf cache (got a new leaf pointer)")
	}
}

func TestGitHubState_SetRejectsBadInput(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	s := NewGitHubState()

	if err := s.Set(certPEM, keyPEM, "", 0); err == nil {
		t.Errorf("Set with empty token: want error, got nil")
	}
	if err := s.Set([]byte("not a pem"), keyPEM, "ghs_x", 0); err == nil {
		t.Errorf("Set with bad cert PEM: want error, got nil")
	}
	if err := s.Set(certPEM, []byte("not a pem"), "ghs_x", 0); err == nil {
		t.Errorf("Set with bad key PEM: want error, got nil")
	}
	// A failed Set must not have clobbered the (still empty) state.
	if tok, _ := s.Token(); tok != "" {
		t.Errorf("state mutated by a failed Set: token = %q", tok)
	}
}

func TestGitHubState_RecordRejection_LatchAndConsume(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	s := NewGitHubState()
	if err := s.Set(certPEM, keyPEM, "ghs_live", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// A rejection of a token that is no longer current is ignored (it is already superseded).
	s.RecordRejection("ghs_stale")
	if s.WaitRejection(context.Background(), 20*time.Millisecond) {
		t.Fatalf("stale-token rejection was latched, want ignored")
	}

	// A rejection of the CURRENT token latches, and the first waiter consumes it.
	s.RecordRejection("ghs_live")
	if !s.WaitRejection(context.Background(), time.Second) {
		t.Fatalf("WaitRejection: want true (latched), got false")
	}
	if s.WaitRejection(context.Background(), 20*time.Millisecond) {
		t.Fatalf("WaitRejection consumed twice: the latch is not edge-triggered")
	}
}

func TestGitHubState_RecordRejection_WakesBlockedWaiter(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	s := NewGitHubState()
	if err := s.Set(certPEM, keyPEM, "ghs_live", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got := make(chan bool, 1)
	go func() { got <- s.WaitRejection(context.Background(), 5*time.Second) }()

	// Give the waiter a moment to register, then reject.
	time.Sleep(20 * time.Millisecond)
	s.RecordRejection("ghs_live")

	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("blocked waiter returned false, want true")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("blocked waiter was never woken by RecordRejection")
	}
}

func TestGitHubState_WaitRejection_TimeoutAndCancel(t *testing.T) {
	s := NewGitHubState()

	start := time.Now()
	if s.WaitRejection(context.Background(), 50*time.Millisecond) {
		t.Fatalf("WaitRejection with no rejection: want false")
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("WaitRejection returned after %v, want it to honour the ~50ms timeout", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- s.WaitRejection(ctx, time.Hour) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatalf("cancelled WaitRejection returned true, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("WaitRejection ignored ctx cancellation")
	}
}

func TestGitHubState_SetClearsPendingRejection(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	s := NewGitHubState()
	if err := s.Set(certPEM, keyPEM, "ghs_old", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	s.RecordRejection("ghs_old")

	// The node pushed a replacement before anyone read the latch: the rejection is moot.
	if err := s.Set(certPEM, keyPEM, "ghs_new", 0); err != nil {
		t.Fatalf("Set (replace): %v", err)
	}
	if s.WaitRejection(context.Background(), 20*time.Millisecond) {
		t.Fatalf("a push did not clear the pending rejection latch")
	}
}
