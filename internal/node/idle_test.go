package node

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
)

// The pump tracks last-relay-activity (a frame in EITHER direction refreshes it) and whether any
// client is attached — the two inputs the idle reaper needs. Covers sp-8hf item 3 (activity side).
func TestPumpTracksActivityAndAttached(t *testing.T) {
	p := newPump(io.Discard, strings.NewReader(""))

	// A fresh pump starts "active now" so it isn't instantly idle.
	if time.Since(p.lastActive()) > time.Second {
		t.Fatal("new pump should start with recent activity")
	}

	// An agent->client frame refreshes activity.
	old := time.Now().Add(-time.Hour)
	p.mu.Lock()
	p.lastActivity = old
	p.mu.Unlock()
	p.appendFrames([]Frame{{Kind: "agent", Text: "hi"}})
	if !p.lastActive().After(old) {
		t.Fatal("appendFrames (agent->client) must refresh activity")
	}

	// A client->pump frame refreshes activity too (the inbound relay direction).
	p.mu.Lock()
	p.lastActivity = old
	p.mu.Unlock()
	p.fromClient("c1", encodeFrame(Frame{Kind: "perm_response", ReqID: "nope", Allow: false}))
	if !p.lastActive().After(old) {
		t.Fatal("fromClient (client->pump) must refresh activity")
	}

	// attached() reflects whether any client is open.
	if p.attached() {
		t.Fatal("no clients -> not attached")
	}
	p.attachClient("c1", 0, func([]byte) error { return nil })
	if !p.attached() {
		t.Fatal("client open -> attached")
	}
}

// reapIdle tears down spawns idle past their stage threshold: a DETACHED spawn gets a short budget; an
// ATTACHED spawn gets a longer one. Covers sp-8hf item 3 (reap side).
func TestReapIdleTwoStage(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	a := newAttacher(newGooseManager(t, be), &fakeCPStream{})
	ctx := context.Background()

	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "idle", AppRef: writeNodeApp(t), Model: "m"})
	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "kept", AppRef: writeNodeApp(t), Model: "m"})
	a.attachClient("kept", SessionZeroID, "c1", 0) // "kept" has a live client -> the longer idle budget

	now := time.Now()
	for _, id := range []string{"idle", "kept"} {
		p := a.pumps[zeroKey(id)]
		p.mu.Lock()
		p.lastActivity = now.Add(-10 * time.Minute)
		p.mu.Unlock()
	}

	// detached budget 5m (idle 10m -> reap), attached budget 30m (idle 10m -> keep).
	a.reapIdle(ctx, now, 5*time.Minute, 30*time.Minute)

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pumps[zeroKey("idle")] != nil {
		t.Fatal("detached spawn idle past its budget must be reaped")
	}
	if a.pumps[zeroKey("kept")] == nil {
		t.Fatal("attached spawn within its longer budget must survive")
	}
}
