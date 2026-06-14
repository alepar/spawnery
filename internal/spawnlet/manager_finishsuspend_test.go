package spawnlet

// manager_finishsuspend_test.go: tests for SnapshotForSuspend (gate) + FinishSuspend (spec §4,
// task sp-ei4.1.15.3).
//
// Test matrix:
//   FS1: SnapshotFailureAborts — snapshot error → no pod.Stop, agent paused-then-unpaused,
//        spawn still in store, journal watchers restarted, error returned.
//   FS2: SnapshotSuccessLeavesPaused — snapshot succeeds → agent paused BEFORE snapshot and
//        NOT unpaused, gate doesn't close journal, spawn still in store, journalState persisted.
//   FS3: FinishSuspendTearsDown — after successful gate, FinishSuspend stops pod, drops spawn
//        from store, closes journal.
//   FS4: FinishSuspendCapturesRootfsDelta — DeltaCapture=true + FinishSuspend → CaptureDelta
//        fires and pod.Stop follows.
//   FS5: GateOnUnknownID — returns error without side-effects.
//   FS5b: GateOnScratchOnlySpawn — no journaler configured → empty markers, agent paused,
//         FinishSuspend tears down cleanly.
//   FS6: FinishSuspendOnUnknownID — returns error.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// FS1: SnapshotForSuspend with a journal error → abort/restore: no pod.Stop, agent
// paused-then-unpaused, spawn stays in store with watchers restarted.
func TestSnapshotForSuspendFailureAborts(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	app := writeJournalApp(t)

	fj := newFakeJournal("manifest-abc")
	fj.finalErr = errors.New("kopia: repo unavailable (injected)")

	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	m.SetJournal(fj, stateDir)

	sp, err := m.Create(ctx, "sp-abort", app, "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, gateErr := m.SnapshotForSuspend(ctx, sp.ID, nil)
	if gateErr == nil {
		t.Fatal("SnapshotForSuspend with a failing journal must return an error")
	}

	// No pod.Stop must have been called.
	if fb.stopped != nil {
		t.Fatalf("pod.Stop must NOT be called on snapshot failure; stopped=%+v", fb.stopped)
	}

	// Agent must have been paused then unpaused (abort path).
	if fb.pauseCount != 1 {
		t.Fatalf("Pause must be called exactly once; got pauseCount=%d", fb.pauseCount)
	}
	if fb.unpauseCount != 1 {
		t.Fatalf("Unpause must be called exactly once on abort; got unpauseCount=%d", fb.unpauseCount)
	}

	// Spawn must still be in the store (never claimed).
	sp2, live := m.store.Get(sp.ID)
	if !live {
		t.Fatal("spawn must still be in the store after a failed gate")
	}

	// Journal watchers must have been restarted (abort restores the live state).
	if len(sp2.journalWatchers) == 0 {
		t.Fatal("journal watchers must be restarted after a failed snapshot gate")
	}

	// journalState must NOT have been written (snapshot never succeeded).
	stateFile := filepath.Join(stateDir, sp.ID+".json")
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Fatalf("journalState must NOT be written on snapshot failure; stat err=%v", err)
	}

	// Clean up: stop the spawn so the restarted watchers are torn down.
	_ = m.Stop(ctx, sp.ID)
}

// FS2: SnapshotForSuspend success → agent paused BEFORE snapshot, NOT unpaused after,
// gate does NOT call journal.Close, spawn stays in store, journalState persisted.
func TestSnapshotForSuspendSuccessLeavesPaused(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	app := writeJournalApp(t)

	fb := &fakePodBackend{}

	fj := newFakeJournal("manifest-abc")
	var pausedDuringSnapshot bool
	fj.onFinal = func() {
		// Record whether the agent was paused at the moment FinalSnapshot fires.
		pausedDuringSnapshot = fb.paused
	}

	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	m.SetJournal(fj, stateDir)

	sp, err := m.Create(ctx, "sp-pause", app, "model", "", "", 2)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := m.SnapshotForSuspend(ctx, sp.ID, nil)
	if err != nil {
		t.Fatalf("SnapshotForSuspend: %v", err)
	}

	// Markers must be populated.
	if result.MountMarkers["main"] != "manifest-abc" {
		t.Fatalf("MountMarkers = %v, want {main: manifest-abc}", result.MountMarkers)
	}

	// Agent must have been paused BEFORE the snapshot was taken.
	if !pausedDuringSnapshot {
		t.Fatal("agent must be paused BEFORE FinalSnapshot fires")
	}

	// Agent must NOT have been unpaused (left paused for FinishSuspend to call pod.Stop on).
	if fb.unpauseCount != 0 {
		t.Fatalf("agent must NOT be unpaused on success; got unpauseCount=%d", fb.unpauseCount)
	}

	// Gate must NOT call journal.Close (Close stays in FinishSuspend's teardown).
	if fj.closeCount != 0 {
		t.Fatalf("gate must NOT call journal.Close; got closeCount=%d", fj.closeCount)
	}

	// Spawn must still be in the store (gate is non-destructive).
	if _, live := m.store.Get(sp.ID); !live {
		t.Fatal("spawn must still be in the store after a successful gate")
	}

	// journalState must have been persisted by the gate.
	stateFile := filepath.Join(stateDir, sp.ID+".json")
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("journalState must be persisted on successful gate: %v", err)
	}

	// Clean up via FinishSuspend (not Stop, to match the two-phase flow).
	if _, err := m.FinishSuspend(ctx, sp.ID, false, nil); err != nil {
		t.Fatalf("FinishSuspend cleanup: %v", err)
	}
}

// FS3: FinishSuspend after a successful gate → pod.Stop called, spawn dropped from store,
// journal.Close called.
func TestFinishSuspendTearsDown(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	app := writeJournalApp(t)

	fj := newFakeJournal("manifest-xyz")
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	m.SetJournal(fj, stateDir)

	sp, err := m.Create(ctx, "sp-finish", app, "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Gate first.
	if _, err := m.SnapshotForSuspend(ctx, sp.ID, nil); err != nil {
		t.Fatalf("SnapshotForSuspend: %v", err)
	}

	// Finish: must tear down.
	if _, err := m.FinishSuspend(ctx, sp.ID, false, nil); err != nil {
		t.Fatalf("FinishSuspend: %v", err)
	}

	// Pod must have been stopped.
	if fb.stopped == nil {
		t.Fatal("pod.Stop must be called by FinishSuspend")
	}

	// Spawn must be dropped from the store.
	if _, live := m.store.Get(sp.ID); live {
		t.Fatal("spawn must be removed from store after FinishSuspend")
	}

	// journal.Close must have been called (teardown closes the repo).
	if fj.closeCount == 0 {
		t.Fatal("journal.Close must be called by FinishSuspend")
	}

	// Mount dirs must be finalized (host dirs removed by Scratch.Finalize).
	for _, d := range sp.MountDirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("mount dir %s must be removed by FinishSuspend; stat err=%v", d, err)
		}
	}
}

// FS4: FinishSuspend with DeltaCapture=true captures the rootfs delta before pod.Stop.
func TestFinishSuspendCapturesRootfsDelta(t *testing.T) {
	ctx := context.Background()
	app := writeJournalApp(t)

	fj := newFakeJournal("manifest-delta")
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())
	// Stub scrubFn so the hermetic test never shells out to Docker.
	m.scrubFn = func(_ context.Context, _ string, _ []string) error { return nil }

	sp, err := m.Create(ctx, "sp-delta-finish", app, "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Gate.
	if _, err := m.SnapshotForSuspend(ctx, sp.ID, nil); err != nil {
		t.Fatalf("SnapshotForSuspend: %v", err)
	}

	// Finish with captureRootfsArtifact=false (rootfs export to journal not needed here).
	if _, err := m.FinishSuspend(ctx, sp.ID, false, nil); err != nil {
		t.Fatalf("FinishSuspend: %v", err)
	}

	// CaptureDelta must have been called.
	if fb.capturedRef == "" {
		t.Fatal("CaptureDelta must be called by FinishSuspend when DeltaCapture=true")
	}

	// pod.Stop must have been called AFTER CaptureDelta.
	captureIdx := opsIndex(fb.ops, "capture:"+sp.ID)
	stopIdx := opsIndex(fb.ops, "stop")
	if captureIdx < 0 {
		t.Fatalf("ops missing capture; ops=%v", fb.ops)
	}
	if stopIdx < 0 {
		t.Fatalf("ops missing stop; ops=%v", fb.ops)
	}
	if captureIdx >= stopIdx {
		t.Fatalf("CaptureDelta must happen BEFORE pod.Stop; ops=%v", fb.ops)
	}
}

// FS5: Gate on an unknown spawn ID returns an error without side-effects.
func TestSnapshotForSuspendUnknownID(t *testing.T) {
	m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	_, err := m.SnapshotForSuspend(context.Background(), "no-such-spawn", nil)
	if err == nil {
		t.Fatal("SnapshotForSuspend on unknown id must return an error")
	}
}

// FS5b: Gate on a scratch-only spawn (no journaler) → empty markers, agent paused,
// FinishSuspend tears down cleanly.
func TestSnapshotForSuspendScratchOnly(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	// No journal installed → scratch-only behavior.

	sp, err := m.Create(ctx, "sp-scratch-gate", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := m.SnapshotForSuspend(ctx, sp.ID, nil)
	if err != nil {
		t.Fatalf("SnapshotForSuspend on scratch-only spawn: %v", err)
	}
	if len(result.MountMarkers) != 0 {
		t.Fatalf("scratch-only gate must return empty markers; got %v", result.MountMarkers)
	}

	// Agent must have been paused.
	if fb.pauseCount != 1 {
		t.Fatalf("Pause must be called; got pauseCount=%d", fb.pauseCount)
	}
	// Not unpaused (success path).
	if fb.unpauseCount != 0 {
		t.Fatalf("agent must NOT be unpaused on success; got unpauseCount=%d", fb.unpauseCount)
	}

	// Spawn still in store.
	if _, live := m.store.Get(sp.ID); !live {
		t.Fatal("spawn must still be in store after successful gate")
	}

	// FinishSuspend must tear down cleanly.
	if _, err := m.FinishSuspend(ctx, sp.ID, false, nil); err != nil {
		t.Fatalf("FinishSuspend: %v", err)
	}
	if fb.stopped == nil {
		t.Fatal("pod.Stop must be called by FinishSuspend")
	}
	if _, live := m.store.Get(sp.ID); live {
		t.Fatal("spawn must be removed from store after FinishSuspend")
	}
}

// FS6: FinishSuspend on an unknown spawn ID returns an error.
func TestFinishSuspendUnknownID(t *testing.T) {
	m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	_, err := m.FinishSuspend(context.Background(), "no-such-spawn", false, nil)
	if err == nil {
		t.Fatal("FinishSuspend on unknown id must return an error")
	}
}

// FS7 (regression, scrub-on-paused): the gate (SnapshotForSuspend) pauses the agent for journal
// quiescence and leaves it paused; FinishSuspend must UNPAUSE it before the rootfs scrub/capture,
// because the scrub `docker exec` cannot run on a paused container ("container is paused").
func TestFinishSuspendUnpausesAgentBeforeCapture(t *testing.T) {
	ctx := context.Background()
	app := writeJournalApp(t)

	fj := newFakeJournal("manifest-unpause")
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())
	m.scrubFn = func(_ context.Context, _ string, _ []string) error { return nil }

	sp, err := m.Create(ctx, "sp-unpause", app, "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.SnapshotForSuspend(ctx, sp.ID, nil); err != nil {
		t.Fatalf("SnapshotForSuspend: %v", err)
	}
	if !fb.paused {
		t.Fatal("gate must leave the agent paused for journal quiescence")
	}
	if _, err := m.FinishSuspend(ctx, sp.ID, false, nil); err != nil {
		t.Fatalf("FinishSuspend: %v", err)
	}
	if fb.unpauseCount == 0 {
		t.Fatal("FinishSuspend must unpause the agent before the rootfs scrub/capture")
	}
	if fb.paused {
		t.Fatal("agent still paused after FinishSuspend — scrub/capture/stop would run on a frozen container")
	}
}
