package spawnlet

// manager_watcher_race_test.go: hermetic race test for the sp.journalWatchers data race
// fixed by sp-csks (watchersMu on Manager).
//
// The race: SnapshotForSuspend uses store.Get (non-destructive; spawn stays in the store),
// while Stop concurrently uses store.Claim (removes the spawn). Both goroutines end up
// with the SAME *Spawn pointer and touch sp.journalWatchers without synchronization:
//   - SnapshotForSuspend: reads sp.journalWatchers for range, then writes nil to it (or
//     writes a fresh watcher slice on abort).
//   - teardown (via Stop): reads sp.journalWatchers for range.
//
// Run with: go test -race ./internal/spawnlet/... -run TestSnapshotForSuspendRaceVsStop
// Before the watchersMu fix this reliably trips the race detector; after the fix it passes.

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestSnapshotForSuspendRaceVsStop fires SnapshotForSuspend and Stop concurrently on the
// same journaled spawn, verifying that the watchersMu fix eliminates the data race on
// sp.journalWatchers (sp-csks). The test creates a new spawn per iteration so each
// iteration is independent; running -race catches unsynchronized accesses from either
// goroutine across the iteration batch.
func TestSnapshotForSuspendRaceVsStop(t *testing.T) {
	ctx := context.Background()
	app := writeJournalApp(t)

	// 200 iterations substantially increases race-detector coverage: each iteration fires
	// two goroutines that both hold the same *Spawn pointer (Get vs Claim) and race on
	// sp.journalWatchers. Without watchersMu the detector catches the race within the
	// first few iterations; with it every iteration is clean.
	const iterations = 200
	for i := 0; i < iterations; i++ {
		fj := newFakeJournal("manifest-race")
		fb := &fakePodBackend{}
		m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
			AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		})
		m.SetJournal(fj, t.TempDir())

		id := fmt.Sprintf("sp-race-%d", i)
		if _, err := m.Create(ctx, id, app, "model", "", "", uint64(i+1)); err != nil {
			t.Fatalf("iter %d Create: %v", i, err)
		}

		// Race goroutine A: SnapshotForSuspend calls store.Get (non-destructive) then
		// touches sp.journalWatchers (stops watchers, writes nil or restarts on abort).
		// Race goroutine B: Stop calls store.Claim (removes from store) then teardown
		// ranges over sp.journalWatchers — same pointer as A's Get result.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = m.SnapshotForSuspend(ctx, id, nil)
		}()
		go func() {
			defer wg.Done()
			_ = m.Stop(ctx, id)
		}()
		wg.Wait()

		// Clean up any spawn left in-store if SnapshotForSuspend won both races
		// (got sp via Get AND the snapshot succeeded, leaving spawn for FinishSuspend).
		// Stop is idempotent: returns an error on unknown id, never panics.
		_ = m.Stop(ctx, id)
	}
}
