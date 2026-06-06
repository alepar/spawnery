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

type fakeSender struct {
	mu   sync.Mutex
	sent []*nodev1.CPMessage
}

func (f *fakeSender) Send(m *nodev1.CPMessage) error {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) first() *nodev1.CPMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	return f.sent[0]
}

func TestProvisionRoutesAndAwaitsActive(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 2*time.Second)

	send := &fakeSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	go func() {
		for {
			if m := send.first(); m != nil {
				s.OnStatus(m.GetStart().GetSpawnId(), nodev1.SpawnPhase_ACTIVE)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	nodeID, err := s.Provision(context.Background(), "sp-test", "examples/secret-app", "m", "", "", registry.Placement{})
	if err != nil {
		t.Fatal(err)
	}
	if nodeID != "n1" {
		t.Fatalf("provision node=%q", nodeID)
	}
	got := send.first().GetStart()
	if got.GetSpawnId() != "sp-test" || got.GetAppRef() != "examples/secret-app" || got.GetModel() != "m" {
		t.Fatalf("StartSpawn payload wrong: %+v", got)
	}
}

func TestProvisionNoCapacity(t *testing.T) {
	s := New(registry.New(), router.New(), time.Second)
	if _, err := s.Provision(context.Background(), "sp-x", "ref", "m", "", "", registry.Placement{}); err == nil {
		t.Fatal("expected ResourceExhausted when no node")
	}
}
