package node

import (
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

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
