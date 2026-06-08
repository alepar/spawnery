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
	if _, err := rt.AttachClient("sp1", "0", "c1", cl, 0); err != nil {
		t.Fatal(err)
	}
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{SpawnId: "sp1", ClientId: "c1", Data: []byte("hi")}}}

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
	waitActive(t, s, id) // async CreateSpawn — wait for background provision to complete

	live, err := s.st.Spawns().LiveContainersByNode(ctx, "n1")
	if err != nil || len(live) != 1 || live[0].SpawnID != id {
		t.Fatalf("LiveContainersByNode(n1)=%+v err=%v want [%s]", live, err, id)
	}
	got, _ := s.st.Spawns().Get(ctx, id)
	if got.Status != store.Active {
		t.Fatalf("status=%v want active", got.Status)
	}
}

// A node-reported SessionRoster is mirrored into the CP router so ListSessions can serve it.
// (recvFromChan adapter lives in node_class_test.go.) runNode is kept alive in a goroutine: letting it
// return would run its deferred DropNode and tear down the route under test.
func TestRunNodeMirrorsRoster(t *testing.T) {
	s, _, rt := newTestServer(t)
	sender := &capSender{}
	rt.Bind("s1", "node-1", sender)
	in := make(chan *nodev1.NodeMessage, 2)
	go s.runNode(context.Background(), sender, recvFromChan(in))
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: "node-1"}}}
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Roster{Roster: &nodev1.SessionRoster{
		SpawnId: "s1", Sessions: []*nodev1.SessionInfo{{SessionId: "0", State: nodev1.SessionState_SESSION_STATE_ACTIVE, Pinned: true}},
	}}}
	deadline := time.Now().Add(time.Second)
	for {
		if got := rt.ListSessions("s1"); len(got) == 1 && got[0].Pinned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("roster never mirrored: %+v", rt.ListSessions("s1"))
		}
		time.Sleep(time.Millisecond)
	}
	close(in)
}

// The client-facing ListSessions RPC reads the CP's mirrored roster (owner-checked) and maps node
// session state to the client status string.
func TestListSessionsRPC(t *testing.T) {
	s, _, rt := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	now := time.Now().Unix()
	sp := store.Spawn{
		ID: "s1", OwnerID: "alice", Name: "n", AppID: "secret-app", AppVersion: "1.0.0",
		AppRef: "examples/secret-app", Model: "m", CreatedAt: now, LastUsedAt: now,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, nil) }); err != nil {
		t.Fatal(err)
	}
	rt.Bind("s1", "node-1", &capSender{})
	rt.UpdateRoster("s1", []*nodev1.SessionInfo{
		{SessionId: "0", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp", State: nodev1.SessionState_SESSION_STATE_ACTIVE, Pinned: true},
	})
	resp, err := s.ListSessions(ctx, connect.NewRequest(&cpv1.ListSessionsRequest{SpawnId: "s1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Sessions) != 1 || resp.Msg.Sessions[0].Status != "active" || !resp.Msg.Sessions[0].Pinned {
		t.Fatalf("ListSessions RPC wrong: %+v", resp.Msg.Sessions)
	}
	// A foreign caller is denied.
	other := auth.WithOwner(context.Background(), "mallory")
	if _, err := s.ListSessions(other, connect.NewRequest(&cpv1.ListSessionsRequest{SpawnId: "s1"})); err == nil {
		t.Fatalf("ListSessions must reject a non-owner")
	}
}
