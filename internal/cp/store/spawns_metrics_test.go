package store

import (
	"context"
	"testing"
)

// TestCountByStatus verifies that CountByStatus tallies non-deleted spawns per status and
// excludes soft-deleted rows.
func TestCountByStatus(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	// Create spawns in various statuses by driving them through lifecycle transitions.
	// We seed directly via status overrides to keep the test focused.
	createSpawn := func(id string) {
		t.Helper()
		mounts := []Mount{{Name: "main", BackendURI: "scratch"}}
		inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn(id), mounts) })
	}

	createSpawn("s1")
	createSpawn("s2")
	createSpawn("s3")
	createSpawn("s4")
	createSpawn("s5")

	// Drive some spawns to Active.
	if err := st.Spawns().SetActive(ctx, "s1", "node-1", 1); err != nil {
		t.Fatalf("SetActive s1: %v", err)
	}
	if err := st.Spawns().SetActive(ctx, "s2", "node-1", 1); err != nil {
		t.Fatalf("SetActive s2: %v", err)
	}

	// Drive one to Suspended (Starting -> Suspending -> Suspended via direct SetSuspended).
	// SetSuspended requires a live container in suspending phase. Drive s3 manually via the
	// SetSuspending path, then end the container + SetSuspended. Use SetActive first to get a
	// live container, then Suspending, then Suspended.
	if err := st.Spawns().SetActive(ctx, "s3", "node-1", 1); err != nil {
		t.Fatalf("SetActive s3: %v", err)
	}
	if err := st.Spawns().SetSuspending(ctx, "s3", 1); err != nil {
		t.Fatalf("SetSuspending s3: %v", err)
	}
	if err := st.Spawns().SetSuspended(ctx, "s3", 1); err != nil {
		t.Fatalf("SetSuspended s3: %v", err)
	}

	// Soft-delete s5; it must NOT appear in CountByStatus.
	if err := st.Spawns().MarkDeleted(ctx, "s5", 999); err != nil {
		t.Fatalf("MarkDeleted s5: %v", err)
	}

	// s4 remains in Starting.

	counts, err := st.Spawns().CountByStatus(ctx)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}

	if got, want := counts[Active], 2; got != want {
		t.Errorf("active count = %d, want %d", got, want)
	}
	if got, want := counts[Starting], 1; got != want {
		t.Errorf("starting count = %d, want %d", got, want)
	}
	if got, want := counts[Suspended], 1; got != want {
		t.Errorf("suspended count = %d, want %d", got, want)
	}
	if _, ok := counts[Deleted]; ok {
		t.Error("Deleted rows must NOT appear in CountByStatus")
	}

	total := 0
	for _, v := range counts {
		total += v
	}
	if total != 4 {
		t.Errorf("total non-deleted spawns = %d, want 4 (s5 is deleted)", total)
	}
}
