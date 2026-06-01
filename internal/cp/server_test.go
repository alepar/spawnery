package cp

import (
	"context"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	nodev1 "spawnery/gen/node/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
)

type capSender struct {
	mu   sync.Mutex
	sent []*nodev1.CPMessage
}

func (c *capSender) Send(m *nodev1.CPMessage) error {
	c.mu.Lock()
	c.sent = append(c.sent, m)
	c.mu.Unlock()
	return nil
}

func (c *capSender) firstStart() *nodev1.StartSpawn {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.sent {
		if st := m.GetStart(); st != nil {
			return st
		}
	}
	return nil
}

func newTestServer(t *testing.T) (*Server, *registry.Registry, *router.Router) {
	return newTestServerSink(t, telemetry.NopSink{})
}

func newTestServerSink(t *testing.T, sink telemetry.Sink) (*Server, *registry.Registry, *router.Router) {
	reg := registry.New()
	rt := router.New()
	sc := scheduler.New(reg, rt, time.Second)
	st := store.NewTestStore(t)
	if err := Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]AppSeed{{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(reg, rt, sc, st, sink)
	return s, reg, rt
}

func TestRunNodeRegistersAndRoutesFrames(t *testing.T) {
	s, reg, rt := newTestServer(t)
	in := make(chan *nodev1.NodeMessage, 8)
	recv := func() (*nodev1.NodeMessage, error) {
		m, ok := <-in
		if !ok {
			return nil, context.Canceled
		}
		return m, nil
	}
	sender := &capSender{}
	go s.runNode(context.Background(), sender, recv)

	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: "n1", MaxSpawns: 1}}}
	cl := &capClient{}
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
	rt.Bind("sp1", "n1", sender)
	if _, err := rt.AttachClient("sp1", cl); err != nil {
		t.Fatal(err)
	}
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{SpawnId: "sp1", Data: []byte("hi")}}}

	deadline = time.Now().Add(time.Second)
	for cl.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("node frame never reached client")
		}
		time.Sleep(time.Millisecond)
	}
	if string(cl.first()) != "hi" {
		t.Fatalf("got %q", cl.first())
	}
	close(in)
}

type capClient struct {
	mu  sync.Mutex
	got [][]byte
}

func (c *capClient) Send(b []byte) error {
	c.mu.Lock()
	c.got = append(c.got, append([]byte(nil), b...))
	c.mu.Unlock()
	return nil
}

func (c *capClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

func (c *capClient) first() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.got) == 0 {
		return nil
	}
	return c.got[0]
}

func TestCreateSpawnPersistsNodeID(t *testing.T) {
	s, reg, _ := newTestServer(t)

	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.SpawnId

	live, err := s.st.Spawns().LiveContainersByNode(ctx, "n1")
	if err != nil || len(live) != 1 || live[0].SpawnID != id {
		t.Fatalf("LiveContainersByNode(n1)=%+v err=%v want [%s]", live, err, id)
	}
	got, _ := s.st.Spawns().Get(ctx, id)
	if got.Status != store.Active {
		t.Fatalf("status=%v want active", got.Status)
	}
}
