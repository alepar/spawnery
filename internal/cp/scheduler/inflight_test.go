package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
)

// blockingSender blocks Send until unblock is closed, letting a test observe scheduler state
// while Provision is mid-flight (waiting for ACTIVE).
type blockingSender struct {
	mu        sync.Mutex
	sent      []*nodev1.CPMessage
	unblock   chan struct{}
	unblocked bool
}

func newBlockingSender() *blockingSender { return &blockingSender{unblock: make(chan struct{})} }

func (b *blockingSender) Send(m *nodev1.CPMessage) error {
	b.mu.Lock()
	b.sent = append(b.sent, m)
	b.mu.Unlock()
	<-b.unblock
	return nil
}

func (b *blockingSender) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.unblocked {
		b.unblocked = true
		close(b.unblock)
	}
}

// TestInFlightStartAbsentBeforeProvision: a spawn id never handed to Provision has no in-flight
// placement recorded.
func TestInFlightStartAbsentBeforeProvision(t *testing.T) {
	s := New(registry.New(), router.New(), time.Second)
	if _, ok := s.InFlightStart("sp-never-provisioned"); ok {
		t.Fatal("expected no in-flight placement before Provision is ever called")
	}
}

// TestInFlightStartPresentDuringProvisionThenRemoved: InFlightStart reports {node, gen} while
// Provision is blocked waiting for ACTIVE, and the entry is gone once Provision returns.
func TestInFlightStartPresentDuringProvisionThenRemoved(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 2*time.Second)

	send := newBlockingSender()
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	done := make(chan struct{})
	var provisionErr error
	go func() {
		_, provisionErr = s.Provision(context.Background(), "sp-inflight", "ref", "m", "", "", "", "", 7,
			registry.Placement{}, nil, nil, "", nil, nil, nil)
		close(done)
	}()

	// Wait for Provision to have sent StartSpawn (and thus registered the in-flight entry) before
	// asserting — Send blocks, so by the time Send is observed to have been called, the in-flight
	// map write (which happens before Send in Provision) has already happened.
	deadline := time.Now().Add(2 * time.Second)
	for {
		send.mu.Lock()
		sent := len(send.sent) > 0
		send.mu.Unlock()
		if sent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for StartSpawn to be sent")
		}
		time.Sleep(time.Millisecond)
	}

	fs, ok := s.InFlightStart("sp-inflight")
	if !ok {
		t.Fatal("expected in-flight placement while Provision is blocked awaiting ACTIVE")
	}
	if fs.NodeID != "n1" || fs.Gen != 7 {
		t.Fatalf("InFlightStart = %+v, want {NodeID: n1, Gen: 7}", fs)
	}

	// Unblock Send, then report ACTIVE so Provision returns.
	send.release()
	s.OnStatus("sp-inflight", nodev1.SpawnPhase_ACTIVE, "")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Provision to return")
	}
	if provisionErr != nil {
		t.Fatalf("Provision returned error: %v", provisionErr)
	}

	if _, ok := s.InFlightStart("sp-inflight"); ok {
		t.Fatal("expected in-flight placement to be removed after Provision returns")
	}
}

// TestInFlightStartRemovedOnProvisionError: the in-flight entry is cleaned up even when Provision
// fails (e.g. a timeout / ERROR from the node), not just on the success path.
func TestInFlightStartRemovedOnProvisionError(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 50*time.Millisecond) // short timeout so the test doesn't hang

	send := &fakeSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	_, err := s.Provision(context.Background(), "sp-timeout", "ref", "m", "", "", "", "", 1,
		registry.Placement{}, nil, nil, "", nil, nil, nil)
	if err == nil {
		t.Fatal("expected Provision to time out (no ACTIVE ever reported)")
	}
	if _, ok := s.InFlightStart("sp-timeout"); ok {
		t.Fatal("expected in-flight placement to be removed after Provision returns (even on error)")
	}
}
