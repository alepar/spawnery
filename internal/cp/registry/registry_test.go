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

// Tenancy: a cloud node is multi-tenant (any owner); a self-hosted node is single-tenant (only its own
// owner's spawns). A spawn for owner O is eligible on any cloud node OR O's own self-hosted node, never
// another owner's self-hosted node.
func TestPickForTenancy(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "cloud1", Free: 5, Class: "cloud"})
	r.Add(&Node{ID: "selfA", Free: 1, Class: "self-hosted", Owner: "alice"})
	r.Add(&Node{ID: "selfB", Free: 9, Class: "self-hosted", Owner: "bob"})

	// bob: eligible on cloud1 + his own selfB; selfB has the most free -> selfB.
	if n := r.PickFor(Placement{Owner: "bob"}); n == nil || n.ID != "selfB" {
		t.Fatalf("bob should pick his own selfB (most free eligible), got %v", n)
	}
	// alice: bob's selfB (most free overall) is NOT eligible -> cloud1 (most free eligible).
	if n := r.PickFor(Placement{Owner: "alice"}); n == nil || n.ID != "cloud1" {
		t.Fatalf("alice must never get bob's self-hosted node; expect cloud1, got %v", n)
	}
	// with cloud full, alice falls back to HER OWN selfA, never bob's selfB.
	r.Heartbeat("cloud1", 5, 0)
	if n := r.PickFor(Placement{Owner: "alice"}); n == nil || n.ID != "selfA" {
		t.Fatalf("alice with cloud full should pick her own selfA, got %v", n)
	}
	// carol owns no self-hosted node; with cloud full there is nothing eligible.
	if n := r.PickFor(Placement{Owner: "carol"}); n != nil {
		t.Fatalf("carol -> nil when only other owners' self-hosted nodes remain, got %v", n)
	}
}
