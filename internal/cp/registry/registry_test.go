package registry

import (
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/cpmetrics"
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
	n2, _ := r.Get("n2")
	r.Heartbeat("n2", n2.token, 4, 0) // active=4 free=0 -> now nobody has capacity
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
// another owner's self-hosted node. (Body below under TestPickFor.)
func TestRegisterRejectsLiveButDisplacesDead(t *testing.T) {
	r := New()
	now := time.Unix(1000, 0)
	r.now = func() time.Time { return now }

	t1, ok := r.Register(&Node{ID: "n1", Sender: &fakeSender{}, Free: 1})
	if !ok || t1 == 0 {
		t.Fatalf("first register should be accepted (t1=%d ok=%v)", t1, ok)
	}
	// Still alive -> a duplicate registration is REJECTED.
	if _, ok := r.Register(&Node{ID: "n1", Sender: &fakeSender{}}); ok {
		t.Fatal("duplicate of a live node must be rejected")
	}
	// Go past the live window with no heartbeat -> dead -> the new connection DISPLACES it.
	now = now.Add(r.liveWindow + time.Second)
	t2, ok := r.Register(&Node{ID: "n1", Sender: &fakeSender{}, Free: 1})
	if !ok || t2 == t1 {
		t.Fatalf("dead node should be displaced with a new token (t1=%d t2=%d ok=%v)", t1, t2, ok)
	}
	// Epoch guard: the displaced old token is no longer current and must not tear down the new owner.
	if r.IsCurrent("n1", t1) || !r.IsCurrent("n1", t2) {
		t.Fatal("only the new token should be current")
	}
	if r.RemoveIfCurrent("n1", t1) {
		t.Fatal("old token must not remove the new owner")
	}
	if !r.RemoveIfCurrent("n1", t2) {
		t.Fatal("current token should remove")
	}
	if _, ok := r.Get("n1"); ok {
		t.Fatal("n1 should be gone after the current owner removes it")
	}
}

func TestHeartbeatRefreshesLivenessAndIgnoresStaleToken(t *testing.T) {
	r := New()
	now := time.Unix(2000, 0)
	r.now = func() time.Time { return now }
	t1, _ := r.Register(&Node{ID: "n1", Free: 1})

	r.Heartbeat("n1", 999, 0, 9) // wrong token -> ignored (no free/liveness update)
	if n, _ := r.Get("n1"); n.Free != 1 {
		t.Fatalf("stale-token heartbeat must be ignored, free=%d", n.Free)
	}
	// A correct-token heartbeat near the window edge refreshes liveness, so the node stays alive and a
	// duplicate is still rejected afterwards.
	now = now.Add(r.liveWindow - time.Second)
	r.Heartbeat("n1", t1, 0, 3)
	if n, _ := r.Get("n1"); n.Free != 3 {
		t.Fatalf("heartbeat should set free=3, got %d", n.Free)
	}
	now = now.Add(2 * time.Second) // would be dead from the first register, but the heartbeat refreshed it
	if _, ok := r.Register(&Node{ID: "n1"}); ok {
		t.Fatal("recently-heartbeated node is alive -> duplicate must be rejected")
	}
}

func TestPickFor(t *testing.T) {
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
	// cloud1 is the first Add in this test -> token 1 (master added the token param to Heartbeat).
	r.Heartbeat("cloud1", 1, 5, 0)
	if n := r.PickFor(Placement{Owner: "alice"}); n == nil || n.ID != "selfA" {
		t.Fatalf("alice with cloud full should pick her own selfA, got %v", n)
	}
	// carol owns no self-hosted node; with cloud full there is nothing eligible.
	if n := r.PickFor(Placement{Owner: "carol"}); n != nil {
		t.Fatalf("carol -> nil when only other owners' self-hosted nodes remain, got %v", n)
	}
}

func TestPickForImage(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "a", Max: 1, Free: 1, Images: []string{"img:goose"}})
	r.Add(&Node{ID: "b", Max: 1, Free: 1, Images: []string{"img:opencode"}})

	if n := r.PickFor(Placement{Image: "img:goose"}); n == nil || n.ID != "a" {
		t.Fatalf("want node a for img:goose, got %v", n)
	}
	if n := r.PickFor(Placement{Image: "img:opencode"}); n == nil || n.ID != "b" {
		t.Fatalf("want node b for img:opencode, got %v", n)
	}
	if n := r.PickFor(Placement{Image: "img:missing"}); n != nil {
		t.Fatalf("want nil for an image no node has, got %v", n)
	}
	if n := r.PickFor(Placement{}); n == nil {
		t.Fatalf("empty placement should still pick a node")
	}
}

// TestMetricsSnapshot verifies that MetricsSnapshot tallies node count and free slots per class.
func TestMetricsSnapshot(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "c1", Class: "cloud", Free: 3})
	r.Add(&Node{ID: "c2", Class: "cloud", Free: 5})
	r.Add(&Node{ID: "sh1", Class: "self-hosted", Free: 2})

	snap := r.MetricsSnapshot()

	want := map[string]cpmetrics.NodeClassStat{
		"cloud":       {Nodes: 2, FreeSlots: 8},
		"self-hosted": {Nodes: 1, FreeSlots: 2},
	}
	for class, ws := range want {
		gs, ok := snap[class]
		if !ok {
			t.Errorf("class %q missing from snapshot", class)
			continue
		}
		if gs.Nodes != ws.Nodes || gs.FreeSlots != ws.FreeSlots {
			t.Errorf("class %q: got {Nodes:%d FreeSlots:%d}, want {Nodes:%d FreeSlots:%d}",
				class, gs.Nodes, gs.FreeSlots, ws.Nodes, ws.FreeSlots)
		}
	}
	if len(snap) != len(want) {
		t.Errorf("snapshot has %d classes, want %d", len(snap), len(want))
	}
}

// TestMetricsSnapshotDefaultClass verifies that nodes with no Class set are bucketed as "default".
func TestMetricsSnapshotDefaultClass(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "n1", Class: "", Free: 4})
	snap := r.MetricsSnapshot()
	if s, ok := snap["default"]; !ok || s.Nodes != 1 || s.FreeSlots != 4 {
		t.Errorf("empty-class node should appear as 'default': %+v", snap)
	}
}

func TestRegistryPlacementHonorsMinDiskFreeBytes(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "too-small", Max: 1, Free: 9, DiskFreeBytes: 299})
	r.Add(&Node{ID: "enough", Max: 1, Free: 1, DiskFreeBytes: 300})

	if n := r.PickFor(Placement{MinDiskFreeBytes: 300}); n == nil || n.ID != "enough" {
		t.Fatalf("PickFor MinDiskFreeBytes=300 = %+v, want enough", n)
	}
	if n := r.PickFor(Placement{TargetNodeID: "too-small", MinDiskFreeBytes: 300}); n != nil {
		t.Fatalf("forced target with insufficient disk picked %+v, want nil", n)
	}
	if n := r.PickFor(Placement{TargetNodeID: "missing-telemetry", MinDiskFreeBytes: 1}); n != nil {
		t.Fatalf("unknown target picked %+v, want nil", n)
	}
	r.Add(&Node{ID: "missing-telemetry", Max: 1, Free: 1})
	if n := r.PickFor(Placement{TargetNodeID: "missing-telemetry", MinDiskFreeBytes: 1}); n != nil {
		t.Fatalf("node without disk telemetry picked %+v, want fail-closed nil", n)
	}
}
