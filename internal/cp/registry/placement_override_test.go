package registry

import "testing"

func TestPickForTargetNodeID(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "n1", Max: 10, Free: 10})
	r.Add(&Node{ID: "n2", Max: 1, Free: 1}) // less free, but the forced target

	// Without an override, the most-free node wins.
	if got := r.PickFor(Placement{}); got == nil || got.ID != "n1" {
		t.Fatalf("PickFor unrestricted = %v, want n1", got)
	}
	// Forced to n2, even though n1 has more capacity.
	if got := r.PickFor(Placement{TargetNodeID: "n2"}); got == nil || got.ID != "n2" {
		t.Fatalf("PickFor TargetNodeID=n2 = %v, want n2", got)
	}
	// Forced to a node with no capacity -> nil.
	r.Add(&Node{ID: "full", Max: 1, Free: 0})
	if got := r.PickFor(Placement{TargetNodeID: "full"}); got != nil {
		t.Fatalf("PickFor a full target = %v, want nil", got)
	}
	// Forced to an unknown node -> nil.
	if got := r.PickFor(Placement{TargetNodeID: "ghost"}); got != nil {
		t.Fatalf("PickFor unknown target = %v, want nil", got)
	}
}

func TestPickForRequireClass(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "self", Class: "self-hosted", Owner: "alice", Max: 10, Free: 10})
	r.Add(&Node{ID: "cloud", Class: "cloud", Max: 1, Free: 1})

	// Restrict to cloud: the self-hosted node (more free) is excluded.
	got := r.PickFor(Placement{Owner: "alice", RequireClass: "cloud"})
	if got == nil || got.ID != "cloud" {
		t.Fatalf("PickFor RequireClass=cloud = %v, want cloud", got)
	}
	// No node of the required class -> nil.
	if got := r.PickFor(Placement{Owner: "alice", RequireClass: "gpu"}); got != nil {
		t.Fatalf("PickFor RequireClass=gpu = %v, want nil", got)
	}
}

// A forced self-hosted target still has to clear the tenancy rule.
func TestPickForTargetRespectsTenancy(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "bobs", Class: "self-hosted", Owner: "bob", Max: 5, Free: 5})
	if got := r.PickFor(Placement{Owner: "alice", TargetNodeID: "bobs"}); got != nil {
		t.Fatalf("PickFor foreign self-hosted target = %v, want nil (tenancy)", got)
	}
	if got := r.PickFor(Placement{Owner: "bob", TargetNodeID: "bobs"}); got == nil || got.ID != "bobs" {
		t.Fatalf("PickFor own self-hosted target = %v, want bobs", got)
	}
}

func TestTargetEligible(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "cloud", Class: "cloud", Max: 1, Free: 1})
	r.Add(&Node{ID: "bobs", Class: "self-hosted", Owner: "bob", Max: 1, Free: 1})

	if exists, eligible := r.TargetEligible("ghost", "alice"); exists || eligible {
		t.Fatalf("unknown node: exists=%v eligible=%v, want false/false", exists, eligible)
	}
	if exists, eligible := r.TargetEligible("cloud", "alice"); !exists || !eligible {
		t.Fatalf("cloud node: exists=%v eligible=%v, want true/true", exists, eligible)
	}
	if exists, eligible := r.TargetEligible("bobs", "alice"); !exists || eligible {
		t.Fatalf("foreign self-hosted: exists=%v eligible=%v, want true/false", exists, eligible)
	}
	if exists, eligible := r.TargetEligible("bobs", "bob"); !exists || !eligible {
		t.Fatalf("own self-hosted: exists=%v eligible=%v, want true/true", exists, eligible)
	}
}
