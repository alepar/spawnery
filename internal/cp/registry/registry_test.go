package registry

import (
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

type fakeSender struct{ sent []*nodev1.CPMessage }

func (f *fakeSender) Send(m *nodev1.CPMessage) error { f.sent = append(f.sent, m); return nil }

func TestAddHeartbeatPickEvict(t *testing.T) {
	r := New()
	if r.Pick() != nil {
		t.Fatal("empty registry should pick nothing")
	}
	s1, s2 := &fakeSender{}, &fakeSender{}
	r.Add(&Node{ID: "n1", Sender: s1, Max: 2, Free: 0})
	r.Add(&Node{ID: "n2", Sender: s2, Max: 4, Free: 3})
	// n1 has no free slots, n2 has 3 -> Pick returns n2 (most free).
	if n := r.Pick(); n == nil || n.ID != "n2" {
		t.Fatalf("pick: %+v", n)
	}
	r.Heartbeat("n2", 4, 0) // active=4 free=0 -> now nobody has capacity
	if r.Pick() != nil {
		t.Fatal("no capacity -> pick nil")
	}
	r.Remove("n2")
	if _, ok := r.Get("n2"); ok {
		t.Fatal("n2 should be gone")
	}
}

func TestPickFor(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "cloud1", Free: 5, Class: "cloud"})
	r.Add(&Node{ID: "selfA", Free: 1, Class: "self-hosted", Owner: "alice"})
	r.Add(&Node{ID: "selfB", Free: 9, Class: "self-hosted", Owner: "bob"})

	if n := r.PickFor(Placement{}); n == nil || n.ID != "selfB" {
		t.Fatalf("unconstrained should pick max-free selfB, got %v", n)
	}
	if n := r.PickFor(Placement{Class: "self-hosted", Owner: "alice"}); n == nil || n.ID != "selfA" {
		t.Fatalf("class+owner filter should pick selfA, got %v", n)
	}
	if n := r.PickFor(Placement{Class: "self-hosted", Owner: "carol"}); n != nil {
		t.Fatalf("no node owned by carol -> nil, got %v", n)
	}
	if n := r.PickFor(Placement{Class: "cloud"}); n == nil || n.ID != "cloud1" {
		t.Fatalf("class filter should pick cloud1, got %v", n)
	}
}
