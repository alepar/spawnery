package scheduler

import (
	"context"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
)

type fakeSender struct{ sent []*nodev1.CPMessage }

func (f *fakeSender) Send(m *nodev1.CPMessage) error { f.sent = append(f.sent, m); return nil }

func TestProvisionRoutesAndAwaitsActive(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 2*time.Second)

	send := &fakeSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	go func() {
		for {
			if len(send.sent) > 0 {
				id := send.sent[0].GetStart().GetSpawnId()
				s.OnStatus(id, nodev1.SpawnPhase_ACTIVE)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	nodeID, err := s.Provision(context.Background(), "sp-test", "examples/secret-app", "m")
	if err != nil {
		t.Fatal(err)
	}
	if nodeID != "n1" {
		t.Fatalf("provision node=%q", nodeID)
	}
	got := send.sent[0].GetStart()
	if got.GetSpawnId() != "sp-test" || got.GetAppRef() != "examples/secret-app" || got.GetModel() != "m" {
		t.Fatalf("StartSpawn payload wrong: %+v", got)
	}
}

func TestProvisionNoCapacity(t *testing.T) {
	s := New(registry.New(), router.New(), time.Second)
	if _, err := s.Provision(context.Background(), "sp-x", "ref", "m"); err == nil {
		t.Fatal("expected ResourceExhausted when no node")
	}
}
