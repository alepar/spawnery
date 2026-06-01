package cp

import (
	"context"
	"sync"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/telemetry"
)

type captureSink struct {
	mu     sync.Mutex
	events []telemetry.Event
}

func (c *captureSink) Emit(e telemetry.Event) error {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
	return nil
}
func (c *captureSink) find(kind string) (telemetry.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.Kind == kind {
			return e, true
		}
	}
	return telemetry.Event{}, false
}

func feedRegister(in chan *nodev1.NodeMessage, nodeID, class string) {
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: nodeID, MaxSpawns: 1, NodeClass: class}}}
}

func waitNodeClass(t *testing.T, reg *registry.Registry, id, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if n, ok := reg.Get(id); ok && n.Class == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %s did not reach class %q", id, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func recvFromChan(in chan *nodev1.NodeMessage) func() (*nodev1.NodeMessage, error) {
	return func() (*nodev1.NodeMessage, error) {
		m, ok := <-in
		if !ok {
			return nil, context.Canceled
		}
		return m, nil
	}
}

func TestRegisterRecordsNodeClass(t *testing.T) {
	s, reg, _ := newTestServer(t)
	in := make(chan *nodev1.NodeMessage, 4)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	feedRegister(in, "n1", "self-hosted")
	waitNodeClass(t, reg, "n1", "self-hosted")
	feedRegister(in, "n2", "") // empty -> defaults to cloud
	waitNodeClass(t, reg, "n2", "cloud")
	close(in)
}

func TestSpawnCreateTelemetryCarriesNodeClass(t *testing.T) {
	cap := &captureSink{}
	s, reg, _ := newTestServerSink(t, cap)
	in := make(chan *nodev1.NodeMessage, 4)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	feedRegister(in, "n1", "self-hosted")
	// wait for registration
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := reg.Get("n1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never registered")
		}
		time.Sleep(time.Millisecond)
	}
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Status{Status: &nodev1.SpawnStatus{SpawnId: "sp1", Phase: nodev1.SpawnPhase_ACTIVE}}}
	deadline = time.Now().Add(time.Second)
	for {
		if e, ok := cap.find("spawn_create"); ok {
			if e.NodeClass != "self-hosted" {
				t.Fatalf("spawn_create NodeClass = %q (want self-hosted)", e.NodeClass)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no spawn_create telemetry")
		}
		time.Sleep(time.Millisecond)
	}
	close(in)
}
