package node

import (
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

func TestPortAllocatorLowestFreeAndExhaustion(t *testing.T) {
	r := newSessionRegistry("s1")

	p1, ok := r.allocPort("a")
	if !ok || p1 != acpPoolLo {
		t.Fatalf("first allocPort = %d ok=%v, want %d", p1, ok, acpPoolLo)
	}
	p2, _ := r.allocPort("b")
	if p2 != acpPoolLo+1 {
		t.Fatalf("second allocPort = %d, want %d", p2, acpPoolLo+1)
	}
	// freeing p1 makes it the lowest-free again.
	r.freePort(p1, "a")
	p3, _ := r.allocPort("c")
	if p3 != acpPoolLo {
		t.Fatalf("after free, allocPort = %d, want %d (lowest-free)", p3, acpPoolLo)
	}

	// exhaust the pool: 100 ports total; we currently hold p2 + p3, so 98 remain.
	for i := 0; i < 98; i++ {
		if _, ok := r.allocPort("x"); !ok {
			t.Fatalf("unexpected exhaustion at %d", i)
		}
	}
	if _, ok := r.allocPort("x"); ok {
		t.Fatal("allocPort must report exhaustion (ok=false) when the pool is full")
	}
}

func TestSetStateUpdatesSnapshot(t *testing.T) {
	r := newSessionRegistry("s1")
	r.register(&sessionEntry{id: "1", state: nodev1.SessionState_SESSION_STATE_STARTING})
	if !r.setState("1", nodev1.SessionState_SESSION_STATE_ACTIVE) {
		t.Fatal("setState on existing id should return true")
	}
	if r.snapshot()[0].State != nodev1.SessionState_SESSION_STATE_ACTIVE {
		t.Fatal("snapshot did not reflect setState")
	}
	if r.setState("nope", nodev1.SessionState_SESSION_STATE_ACTIVE) {
		t.Fatal("setState on unknown id should return false")
	}
}

func TestSessionRegistry(t *testing.T) {
	r := newSessionRegistry("spawn-1")

	// Session #0 registered with the well-known id, pinned.
	r.register(&sessionEntry{
		id: SessionZeroID, transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP,
		runnable: "goose-acp", state: nodev1.SessionState_SESSION_STATE_ACTIVE, endpoint: "7000", pinned: true,
	})
	if got := r.allocID(); got != "1" {
		t.Fatalf("allocID after #0 = %q, want \"1\"", got)
	}
	r.register(&sessionEntry{id: "1", transport: nodev1.SessionTransport_SESSION_TRANSPORT_MOSH, runnable: "shell", state: nodev1.SessionState_SESSION_STATE_STARTING})
	if r.allocID() != "2" {
		t.Fatalf("allocID after #1 = %q, want \"2\"", r.allocID())
	}

	// snapshot is ordered: session #0 first, then ascending; carries all fields.
	snap := r.snapshot()
	if len(snap) != 2 || snap[0].SessionId != "0" || snap[1].SessionId != "1" {
		t.Fatalf("snapshot order/len wrong: %+v", snap)
	}
	if !snap[0].Pinned || snap[0].Runnable != "goose-acp" {
		t.Fatalf("session #0 fields not preserved: %+v", snap[0])
	}

	// remove a non-pinned session; reflected in snapshot.
	if !r.remove("1") {
		t.Fatalf("remove(\"1\") = false, want true")
	}
	if len(r.snapshot()) != 1 {
		t.Fatalf("after remove len = %d, want 1", len(r.snapshot()))
	}
	if r.remove("nope") {
		t.Fatalf("remove of unknown id returned true")
	}
}
