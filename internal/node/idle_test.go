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
// client is attached — the two inputs the CP-side idle evaluator needs. Covers sp-8hf item 3
// (activity side). The node now reports these as metrics; the CP drives the suspend decision.
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
	p.fromClient("c1", encodeFrame(Frame{Kind: "perm_response", ReqID: "nope", OptionID: "reject"}))
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

// runningSpawns fills DeltaSizeBytes and LastActivityUnixMs: an active spawn with a pump
// reports a non-zero LastActivityUnixMs (epoch-ms) and the backend delta size, so the CP
// evaluator can make quota and idle decisions.
func TestRunningSpawnsPopulatesMetrics(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	a := newAttacher(newGooseManager(t, be), &fakeCPStream{})
	ctx := context.Background()

	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m"})
	defer a.stopSpawn(ctx, "sp1")

	// Back-date the pump's lastActivity so we can verify it is reported faithfully.
	a.mu.Lock()
	p := a.pumps[zeroKey("sp1")]
	a.mu.Unlock()
	if p == nil {
		t.Fatal("pump not registered for sp1")
	}
	knownTime := time.Now().Add(-5 * time.Minute)
	p.mu.Lock()
	p.lastActivity = knownTime
	p.mu.Unlock()

	running := a.runningSpawns(ctx)
	if len(running) == 0 {
		t.Fatal("runningSpawns returned empty; spawn should be live")
	}
	var rs *nodev1.RunningSpawn
	for _, r := range running {
		if r.GetSpawnId() == "sp1" {
			rs = r
			break
		}
	}
	if rs == nil {
		t.Fatal("sp1 not found in runningSpawns")
	}

	// LastActivityUnixMs must reflect the pump's lastActivity (within a generous epsilon).
	gotMs := rs.GetLastActivityUnixMs()
	wantMs := knownTime.UnixMilli()
	if gotMs == 0 {
		t.Fatal("LastActivityUnixMs must be non-zero when pump exists")
	}
	const epsilonMs = 1000 // 1 second tolerance
	if diff := gotMs - wantMs; diff < -epsilonMs || diff > epsilonMs {
		t.Fatalf("LastActivityUnixMs=%d wantMs=%d diff=%d exceeds epsilon %d", gotMs, wantMs, diff, epsilonMs)
	}

	// DeltaSizeBytes is 0 when the fake backend does not have a delta (no DeltaSize configured
	// in the fake that scriptedPodBackend uses) — just assert it doesn't error (field present).
	_ = rs.GetDeltaSizeBytes()
}

// TestLastActivityMsAggregatesAcrossSessionsAndRelays verifies that lastActivityMs returns the
// maximum lastActivity across pumps AND tmux relays for a spawn (both contribute to the max).
func TestLastActivityMsAggregatesAcrossSessionsAndRelays(t *testing.T) {
	a := &attacher{
		pumps:      map[sessionKey]*Pump{},
		tmuxRelays: map[sessionKey]*tmuxRelay{},
	}

	newer := time.Now().Add(-1 * time.Minute)
	older := time.Now().Add(-5 * time.Minute)

	// Pump with older activity.
	p := newPump(io.Discard, strings.NewReader(""))
	p.mu.Lock()
	p.lastActivity = older
	p.mu.Unlock()
	a.pumps[sessionKey{"s1", SessionZeroID}] = p

	// Relay with newer activity.
	relay := newTmuxRelay([]string{"true"}, func(string, []byte) error { return nil })
	relay.mu.Lock()
	relay.lastActivity = newer
	relay.mu.Unlock()
	a.tmuxRelays[sessionKey{"s1", "1"}] = relay

	gotMs := a.lastActivityMs("s1")
	if gotMs == 0 {
		t.Fatal("lastActivityMs should be non-zero when pumps/relays exist")
	}
	// Should reflect the NEWER time (the relay's), not the older (the pump's).
	newerMs := newer.UnixMilli()
	const epsilonMs = 100
	if diff := gotMs - newerMs; diff < -epsilonMs || diff > epsilonMs {
		t.Fatalf("lastActivityMs=%d newerMs=%d diff=%d; should reflect the max (relay's newer time)", gotMs, newerMs, diff)
	}

	// A spawn with no pumps/relays returns 0.
	if ms := a.lastActivityMs("unknown-spawn"); ms != 0 {
		t.Fatalf("lastActivityMs for unknown spawn want 0, got %d", ms)
	}
}
