package router

import (
	"sync"
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

type mcNode struct {
	mu   sync.Mutex
	sent []*nodev1.CPMessage
}

func (n *mcNode) Send(m *nodev1.CPMessage) error {
	n.mu.Lock()
	n.sent = append(n.sent, m)
	n.mu.Unlock()
	return nil
}

func (n *mcNode) opens() (out []*nodev1.SessionOpen) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, m := range n.sent {
		if o := m.GetOpen(); o != nil {
			out = append(out, o)
		}
	}
	return
}

func (n *mcNode) closes() (c int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, m := range n.sent {
		if m.GetClose() != nil {
			c++
		}
	}
	return
}

type mcClient struct {
	mu  sync.Mutex
	got [][]byte
}

func (c *mcClient) Send(b []byte) error {
	c.mu.Lock()
	c.got = append(c.got, b)
	c.mu.Unlock()
	return nil
}

func (c *mcClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

func TestMultiClientFanoutAndPerClientRouting(t *testing.T) {
	r := New()
	node := &mcNode{}
	r.Bind("sp1", "node-1", node)
	a, b := &mcClient{}, &mcClient{}
	if _, err := r.AttachClient("sp1", "0", "ca", "", nil, a, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AttachClient("sp1", "0", "cb", "", nil, b, 7); err != nil {
		t.Fatal(err)
	}
	opens := node.opens()
	if len(opens) != 2 {
		t.Fatalf("want 2 opens, got %d", len(opens))
	}
	if opens[0].ClientId != "ca" || opens[0].Cursor != 0 {
		t.Fatalf("open0 %+v", opens[0])
	}
	if opens[1].ClientId != "cb" || opens[1].Cursor != 7 {
		t.Fatalf("open1 %+v", opens[1])
	}
	r.FromNode("sp1", "0", "ca", []byte("for-a"))
	r.FromNode("sp1", "0", "cb", []byte("for-b"))
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("routing: a=%d b=%d", a.count(), b.count())
	}
	r.DetachClient("sp1", "0", "ca")
	r.DetachClient("sp1", "0", "ca") // stale detach: no-op
	if node.closes() != 1 {
		t.Fatalf("stale detach should send exactly 1 Close, got %d", node.closes())
	}
	r.FromNode("sp1", "0", "ca", []byte("dropped"))
	r.FromNode("sp1", "0", "cb", []byte("still"))
	if a.count() != 1 {
		t.Fatalf("ca should get nothing after detach, got %d", a.count())
	}
	if b.count() != 2 {
		t.Fatalf("cb should still receive, got %d", b.count())
	}
}

func TestFromClientTagsClientID(t *testing.T) {
	r := New()
	node := &mcNode{}
	r.Bind("sp1", "node-1", node)
	a := &mcClient{}
	r.AttachClient("sp1", "0", "ca", "", nil, a, 0)
	if err := r.FromClient("sp1", "0", "ca", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	var fr *nodev1.Frame
	for _, m := range node.sent {
		if f := m.GetFrame(); f != nil {
			fr = f
		}
	}
	if fr == nil || fr.ClientId != "ca" || string(fr.Data) != "hi" {
		t.Fatalf("frame %+v", fr)
	}
}

type fakeNode struct{ sent []*nodev1.CPMessage }

func (f *fakeNode) Send(m *nodev1.CPMessage) error { f.sent = append(f.sent, m); return nil }

type fakeClient struct{ got [][]byte }

func (f *fakeClient) Send(b []byte) error {
	f.got = append(f.got, append([]byte(nil), b...))
	return nil
}

func TestRouteBothWays(t *testing.T) {
	r := New()
	node := &fakeNode{}
	r.Bind("sp1", "n1", node)

	cl := &fakeClient{}
	done, err := r.AttachClient("sp1", "0", "c1", "", nil, cl, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(node.sent) != 1 || node.sent[0].GetOpen().GetSpawnId() != "sp1" {
		t.Fatalf("expected SessionOpen, got %+v", node.sent)
	}

	if err := r.FromClient("sp1", "0", "c1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	last := node.sent[len(node.sent)-1]
	if string(last.GetFrame().GetData()) != "hello" || last.GetFrame().GetSpawnId() != "sp1" {
		t.Fatalf("client->node frame wrong: %+v", last)
	}

	r.FromNode("sp1", "0", "c1", []byte("world"))
	if len(cl.got) != 1 || string(cl.got[0]) != "world" {
		t.Fatalf("node->client: %v", cl.got)
	}

	r.DropNode("n1")
	select {
	case <-done:
	default:
		t.Fatal("done not closed on node drop")
	}
}

func TestUnknownSpawnRoutingIsSafe(t *testing.T) {
	r := New()
	if err := r.FromClient("ghost", "0", "c1", []byte("x")); err == nil {
		t.Fatal("FromClient on unknown spawn should error")
	}
	r.FromNode("ghost", "0", "c1", []byte("x")) // must not panic with no client/route
}

func sessionInfos(ids ...string) []*nodev1.SessionInfo {
	out := make([]*nodev1.SessionInfo, len(ids))
	for i, id := range ids {
		out[i] = &nodev1.SessionInfo{SessionId: id, State: nodev1.SessionState_SESSION_STATE_ACTIVE}
	}
	return out
}

func TestRouterRosterMirror(t *testing.T) {
	r := New()
	n := &mcNode{}
	r.Bind("s1", "node-1", n)
	r.UpdateRoster("s1", "node-1", sessionInfos("0", "1"))
	got := r.ListSessions("s1")
	if len(got) != 2 || got[0].SessionId != "0" {
		t.Fatalf("ListSessions = %+v, want [0 1]", got)
	}
	r.ApplySessionStatus("s1", "1", nodev1.SessionState_SESSION_STATE_CLOSED)
	if r.ListSessions("s1")[1].State != nodev1.SessionState_SESSION_STATE_CLOSED {
		t.Fatalf("ApplySessionStatus did not update the mirror")
	}
}

func TestRouterRosterBeforeBindIsAppliedAtBind(t *testing.T) {
	r := New()
	n := &mcNode{}
	// roster arrives before Bind (race: node emits at ACTIVE; CP Bind runs slightly later)
	r.UpdateRoster("s1", "node-1", sessionInfos("0"))
	if got := r.ListSessions("s1"); got != nil {
		t.Fatalf("unbound spawn must report no sessions, got %+v", got)
	}
	r.Bind("s1", "node-1", n)
	if got := r.ListSessions("s1"); len(got) != 1 || got[0].SessionId != "0" {
		t.Fatalf("pending roster not applied at Bind: %+v", got)
	}
}

func TestListSessionsSnapshotIsNotMutatedByApplySessionStatus(t *testing.T) {
	r := New()
	n := &mcNode{}
	r.Bind("s1", "node-1", n)
	r.UpdateRoster("s1", "node-1", sessionInfos("0"))
	snap := r.ListSessions("s1")
	if len(snap) != 1 || snap[0].State != nodev1.SessionState_SESSION_STATE_ACTIVE {
		t.Fatalf("unexpected snapshot %+v", snap)
	}
	// A subsequent in-place status mutation under the lock must NOT touch the already-returned snapshot.
	r.ApplySessionStatus("s1", "0", nodev1.SessionState_SESSION_STATE_CLOSED)
	if snap[0].State != nodev1.SessionState_SESSION_STATE_ACTIVE {
		t.Fatalf("returned snapshot was mutated by ApplySessionStatus: %+v", snap[0])
	}
	if r.ListSessions("s1")[0].State != nodev1.SessionState_SESSION_STATE_CLOSED {
		t.Fatalf("mirror should reflect the new state on a fresh read")
	}
}

func TestDropNodePurgesPendingRosterForNeverBoundSpawn(t *testing.T) {
	r := New()
	// Roster stashed for a spawn that never binds before its node drops.
	r.UpdateRoster("s1", "node-1", sessionInfos("0"))
	r.DropNode("node-1")
	r.mu.Lock()
	_, leaked := r.pending["s1"]
	r.mu.Unlock()
	if leaked {
		t.Fatal("DropNode leaked a pending roster for a spawn that never bound")
	}
}

func TestRouterCreateCloseSessionSendToNode(t *testing.T) {
	r := New()
	n := &mcNode{}
	r.Bind("s1", "node-1", n)
	if err := r.CreateSession("s1", nodev1.SessionTransport_SESSION_TRANSPORT_MOSH, "shell"); err != nil {
		t.Fatal(err)
	}
	if err := r.CloseSession("s1", "1"); err != nil {
		t.Fatal(err)
	}
	var creates, closes int
	for _, m := range n.sent {
		if m.GetCreateSession() != nil {
			creates++
		}
		if m.GetCloseSession() != nil {
			closes++
		}
	}
	if creates != 1 || closes != 1 {
		t.Fatalf("want 1 CreateSession + 1 CloseSession to the node, got creates=%d closes=%d", creates, closes)
	}
}

// TestPerSessionClientRouting reproduces the multi-session collision (sp-npxq.5): two ACP sessions of
// one spawn whose browser panels share a clientId (the web AcpSessionPanel/TerminalView use a
// module-level CLIENT_ID). The router must key senders by (sessionID, clientID) so session #0's agent
// replies don't get misrouted to the 2nd session's socket. With clientID-only keying, the 2nd attach
// overwrote the first and FromNode for session "0" delivered to session "1"'s client.
func TestPerSessionClientRouting(t *testing.T) {
	r := New()
	node := &mcNode{}
	r.Bind("sp1", "node-1", node)
	s0, s1 := &mcClient{}, &mcClient{}
	if _, err := r.AttachClient("sp1", "0", "shared", "", nil, s0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AttachClient("sp1", "1", "shared", "", nil, s1, 0); err != nil {
		t.Fatal(err)
	}
	r.FromNode("sp1", "0", "shared", []byte("for-0"))
	r.FromNode("sp1", "1", "shared", []byte("for-1"))
	if s0.count() != 1 {
		t.Fatalf("session #0 client must receive its own frame, got %d (misrouted by shared clientID)", s0.count())
	}
	if s1.count() != 1 {
		t.Fatalf("session #1 client must receive its own frame, got %d", s1.count())
	}
	// Detaching session #1 must not drop session #0's same-clientID sender.
	r.DetachClient("sp1", "1", "shared")
	r.FromNode("sp1", "0", "shared", []byte("still-0"))
	if s0.count() != 2 {
		t.Fatalf("session #0 must survive session #1 detach, got %d", s0.count())
	}
}

// TestFromNodeEmptySessionRoutesToZero proves ck() normalizes an EMPTY sessionID to "0":
// a client attached under "0" must receive frames addressed with "" (and vice-versa), so the
// CP's default-session ("") and explicit-session ("0") spellings are the same routing key.
func TestFromNodeEmptySessionRoutesToZero(t *testing.T) {
	r := New()
	node := &mcNode{}
	r.Bind("sp1", "node-1", node)
	c := &mcClient{}
	if _, err := r.AttachClient("sp1", "0", "shared", "", nil, c, 0); err != nil {
		t.Fatal(err)
	}
	// FromNode with an EMPTY sessionID must reach the "0" client.
	r.FromNode("sp1", "", "shared", []byte("for-0"))
	if c.count() != 1 {
		t.Fatalf("empty sessionID must route to the \"0\" client (ck normalizes \"\"->\"0\"), got %d", c.count())
	}
}
