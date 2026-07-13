package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
)

// progressStallSender is a fake NodeSender for start stall-detector tests. On a Start CPMessage it
// records the send and, if driven by the test, lets the caller push Progress()/OnStatus() signals
// on its own schedule (mirroring what the real node does on attach.go's startSpawn path).
type progressStallSender struct {
	mu   sync.Mutex
	sent []*nodev1.CPMessage
}

func (f *progressStallSender) Send(m *nodev1.CPMessage) error {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	return nil
}

func (f *progressStallSender) first() *nodev1.CPMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	return f.sent[0]
}

// TestProvisionSurvivesSlowButProgressingStart is the regression test for the bug: a start that
// takes far longer than the stall window must still succeed as long as the node keeps heartbeating
// progress. This reproduces the 60s-fixed-timeout-vs-2m-apply-report-budget contradiction in
// miniature (sp-mwco.4.8).
func TestProvisionSurvivesSlowButProgressingStart(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 100*time.Millisecond)
	s.backstop = 10 * time.Second

	send := &progressStallSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	const gen = uint64(1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for send.first() == nil {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		// Progress every 40ms for 600ms — 6x the 100ms stall window — then ACTIVE.
		for i := 0; i < 15; i++ {
			time.Sleep(40 * time.Millisecond)
			s.Progress("sp-test", gen)
		}
		s.OnStatus("sp-test", nodev1.SpawnPhase_ACTIVE, "")
	}()

	nodeID, err := s.Provision(context.Background(), "sp-test", "ref", "m", "", "", "", "", gen, registry.Placement{}, nil, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if nodeID != "n1" {
		t.Fatalf("nodeID=%q want n1", nodeID)
	}
}

// TestProvisionStallsWhenNodeGoesQuiet verifies a wedged node (no progress, no status) fails fast —
// within the stall window, well before the old fixed 60s timeout — with a "no progress" message.
func TestProvisionStallsWhenNodeGoesQuiet(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 80*time.Millisecond)
	s.backstop = 10 * time.Second

	send := &progressStallSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	start := time.Now()
	_, err := s.Provision(context.Background(), "sp-test", "ref", "m", "", "", "", "", 1, registry.Placement{}, nil, nil, "", nil, nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected DeadlineExceeded, got nil")
	}
	if elapsed > time.Second {
		t.Fatalf("Provision took %s, want ~80ms (fail fast on stall)", elapsed)
	}
	if !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("err=%q want it to mention \"no progress\"", err.Error())
	}
}

// TestProgressIsGenerationFenced verifies a stale-generation heartbeat does not reset the stall
// timer (must not keep a wedged spawn on a superseded generation alive), that Progress reports the
// mismatch via its bool return, and that Progress on an unknown spawn id is a harmless no-op.
func TestProgressIsGenerationFenced(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 80*time.Millisecond)
	s.backstop = 10 * time.Second

	// Unknown spawn id: no panic, returns false.
	if s.Progress("no-such-spawn", 1) {
		t.Fatal("Progress on unknown spawn id should return false")
	}

	send := &progressStallSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	const gen = uint64(7)
	const staleGen = uint64(6)
	stop := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	var sawFalse, sawTrueForStale bool
	go func() {
		defer close(done)
		deadline := time.Now().Add(2 * time.Second)
		for send.first() == nil {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			matched := s.Progress("sp-test", staleGen)
			mu.Lock()
			if matched {
				sawTrueForStale = true
			} else {
				sawFalse = true
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
		}
	}()

	start := time.Now()
	_, err := s.Provision(context.Background(), "sp-test", "ref", "m", "", "", "", "", gen, registry.Placement{}, nil, nil, "", nil, nil, nil)
	close(stop)
	<-done
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected DeadlineExceeded (stale-gen heartbeats must not keep the spawn alive)")
	}
	if elapsed > time.Second {
		t.Fatalf("Provision took %s, want it to stall despite stale-gen heartbeats", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if sawTrueForStale {
		t.Fatal("Progress with a stale generation returned true; want false")
	}
	if !sawFalse {
		t.Fatal("Progress with a stale generation never returned false")
	}
}

// TestProvisionAbsoluteBackstop guards the infinite-heartbeat wedge: a node that heartbeats forever
// but never reaches ACTIVE must still be killed by the absolute backstop, since the stall detector
// by construction cannot catch it.
func TestProvisionAbsoluteBackstop(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 50*time.Millisecond)
	s.backstop = 200 * time.Millisecond

	send := &progressStallSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	const gen = uint64(1)
	stop := make(chan struct{})
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for send.first() == nil {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.Progress("sp-test", gen)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	start := time.Now()
	_, err := s.Provision(context.Background(), "sp-test", "ref", "m", "", "", "", "", gen, registry.Placement{}, nil, nil, "", nil, nil, nil)
	close(stop)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected DeadlineExceeded from the absolute backstop")
	}
	if elapsed < 150*time.Millisecond || elapsed > time.Second {
		t.Fatalf("Provision took %s, want ~200ms (the backstop, not the stall window)", elapsed)
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err=%q want it to mention \"absolute\"", err.Error())
	}
}
