package router

import (
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

type fakeNode struct{ sent []*nodev1.CPMessage }

func (f *fakeNode) Send(m *nodev1.CPMessage) error { f.sent = append(f.sent, m); return nil }

type fakeClient struct{ got [][]byte }

func (f *fakeClient) Send(b []byte) error { f.got = append(f.got, append([]byte(nil), b...)); return nil }

func TestRouteBothWaysAndOwnership(t *testing.T) {
	r := New()
	node := &fakeNode{}
	r.Bind("sp1", "n1", "alice", node)

	if o, _ := r.Owner("sp1"); o != "alice" {
		t.Fatalf("owner: %q", o)
	}

	cl := &fakeClient{}
	done, err := r.AttachClient("sp1", cl)
	if err != nil {
		t.Fatal(err)
	}
	if len(node.sent) != 1 || node.sent[0].GetOpen().GetSpawnId() != "sp1" {
		t.Fatalf("expected SessionOpen, got %+v", node.sent)
	}

	if err := r.FromClient("sp1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	last := node.sent[len(node.sent)-1]
	if string(last.GetFrame().GetData()) != "hello" || last.GetFrame().GetSpawnId() != "sp1" {
		t.Fatalf("client->node frame wrong: %+v", last)
	}

	r.FromNode("sp1", []byte("world"))
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
	if err := r.FromClient("ghost", []byte("x")); err == nil {
		t.Fatal("FromClient on unknown spawn should error")
	}
	r.FromNode("ghost", []byte("x")) // must not panic with no client/route
}
