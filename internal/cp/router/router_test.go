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
	if _, err := r.AttachClient("sp1", "ca", a, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AttachClient("sp1", "cb", b, 7); err != nil {
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
	r.FromNode("sp1", "ca", []byte("for-a"))
	r.FromNode("sp1", "cb", []byte("for-b"))
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("routing: a=%d b=%d", a.count(), b.count())
	}
	r.DetachClient("sp1", "ca")
	r.DetachClient("sp1", "ca") // stale detach: no-op
	if node.closes() != 1 {
		t.Fatalf("stale detach should send exactly 1 Close, got %d", node.closes())
	}
	r.FromNode("sp1", "ca", []byte("dropped"))
	r.FromNode("sp1", "cb", []byte("still"))
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
	r.AttachClient("sp1", "ca", a, 0)
	if err := r.FromClient("sp1", "ca", []byte("hi")); err != nil {
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

func (f *fakeClient) Send(b []byte) error { f.got = append(f.got, append([]byte(nil), b...)); return nil }

func TestRouteBothWays(t *testing.T) {
	r := New()
	node := &fakeNode{}
	r.Bind("sp1", "n1", node)

	cl := &fakeClient{}
	done, err := r.AttachClient("sp1", "c1", cl, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(node.sent) != 1 || node.sent[0].GetOpen().GetSpawnId() != "sp1" {
		t.Fatalf("expected SessionOpen, got %+v", node.sent)
	}

	if err := r.FromClient("sp1", "c1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	last := node.sent[len(node.sent)-1]
	if string(last.GetFrame().GetData()) != "hello" || last.GetFrame().GetSpawnId() != "sp1" {
		t.Fatalf("client->node frame wrong: %+v", last)
	}

	r.FromNode("sp1", "c1", []byte("world"))
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
	if err := r.FromClient("ghost", "c1", []byte("x")); err == nil {
		t.Fatal("FromClient on unknown spawn should error")
	}
	r.FromNode("ghost", "c1", []byte("x")) // must not panic with no client/route
}
