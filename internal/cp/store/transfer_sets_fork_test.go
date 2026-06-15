package store

import (
	"context"
	"testing"
)

func TestTransferSetForkVariantCarriesSourceAndForkIDs(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	seedTransferSetSpawn(t, st, "sp-source", "alice")
	seedTransferSetSpawn(t, st, "sp-fork", "alice")

	ts := TransferSet{
		ID:                "ts-fork",
		Kind:              TransferSetFork,
		SpawnID:           "sp-fork",
		SourceSpawnID:     "sp-source",
		ForkSpawnID:       "sp-fork",
		SourceGeneration:  7,
		TargetGeneration:  1,
		SourceNodeID:      "node-a",
		TargetNodeID:      "node-b",
		BaseImageDigest:   "sha256:base",
		TransferKeyStatus: TransferKeyPending,
		Status:            TransferSetPending,
		CreatedAt:         100,
		UpdatedAt:         100,
	}
	if err := st.TransferSets().Create(ctx, ts); err != nil {
		t.Fatalf("Create fork transfer set: %v", err)
	}

	got, err := st.TransferSets().Get(ctx, "ts-fork")
	if err != nil {
		t.Fatalf("Get fork transfer set: %v", err)
	}
	if got.Kind != TransferSetFork {
		t.Fatalf("Kind=%q want %q", got.Kind, TransferSetFork)
	}
	if got.SpawnID != "sp-fork" || got.SourceSpawnID != "sp-source" || got.ForkSpawnID != "sp-fork" {
		t.Fatalf("fork transfer-set ids = %+v", got)
	}
	if got.SourceGeneration != 7 || got.TargetGeneration != 1 {
		t.Fatalf("fork transfer-set generations = %+v", got)
	}
}

func TestTransferSetForkVariantValidation(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	seedTransferSetSpawn(t, st, "sp-source", "alice")
	seedTransferSetSpawn(t, st, "sp-fork", "alice")

	err := st.TransferSets().Create(ctx, TransferSet{
		ID:                "ts-bad",
		Kind:              TransferSetFork,
		SpawnID:           "sp-source",
		SourceSpawnID:     "sp-source",
		ForkSpawnID:       "sp-fork",
		SourceGeneration:  7,
		TargetGeneration:  1,
		SourceNodeID:      "node-a",
		TargetNodeID:      "node-b",
		TransferKeyStatus: TransferKeyPending,
		Status:            TransferSetPending,
		CreatedAt:         100,
		UpdatedAt:         100,
	})
	if err == nil {
		t.Fatal("fork transfer set must require SpawnID == ForkSpawnID")
	}
}

func TestGetPendingForkByForkSpawnIDReturnsLatestNonTerminalFork(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"sp-source", "sp-fork", "sp-other"} {
		seedTransferSetSpawn(t, st, id, "alice")
	}

	base := TransferSet{
		Kind:              TransferSetFork,
		SourceSpawnID:     "sp-source",
		SourceGeneration:  7,
		TargetGeneration:  1,
		SourceNodeID:      "node-a",
		TargetNodeID:      "node-b",
		TransferKeyStatus: TransferKeyTargetReady,
	}
	create := func(id, forkID string, status TransferSetStatus, createdAt int64) {
		t.Helper()
		ts := base
		ts.ID = id
		ts.SpawnID = forkID
		ts.ForkSpawnID = forkID
		ts.Status = status
		ts.CreatedAt = createdAt
		ts.UpdatedAt = createdAt
		if err := st.TransferSets().Create(ctx, ts); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	create("ts-old-pending", "sp-fork", TransferSetPending, 10)
	create("ts-active-ignored", "sp-fork", TransferSetActive, 30)
	create("ts-failed-ignored", "sp-fork", TransferSetFailed, 40)
	create("ts-restoring-latest", "sp-fork", TransferSetRestoring, 20)
	create("ts-other", "sp-other", TransferSetRestoring, 50)

	got, err := st.TransferSets().GetPendingForkByForkSpawnID(ctx, "sp-fork")
	if err != nil {
		t.Fatalf("GetPendingForkByForkSpawnID: %v", err)
	}
	if got.ID != "ts-restoring-latest" || got.ForkSpawnID != "sp-fork" || got.TargetNodeID != "node-b" {
		t.Fatalf("pending fork transfer set = %+v", got)
	}
	if _, err := st.TransferSets().GetPendingForkByForkSpawnID(ctx, "sp-missing"); err != ErrNotFound {
		t.Fatalf("missing pending fork error = %v, want ErrNotFound", err)
	}
}

func TestListFailedForksReturnsOnlyVisibleFailedForks(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	for _, id := range []string{
		"sp-source",
		"sp-fork-a",
		"sp-fork-b",
		"sp-fork-active",
		"sp-migration",
		"sp-fork-deleted",
	} {
		seedTransferSetSpawn(t, st, id, "alice")
	}

	create := func(ts TransferSet) {
		t.Helper()
		if err := st.TransferSets().Create(ctx, ts); err != nil {
			t.Fatalf("Create %s: %v", ts.ID, err)
		}
	}
	base := TransferSet{
		SourceSpawnID:     "sp-source",
		SourceGeneration:  7,
		TargetGeneration:  1,
		SourceNodeID:      "node-a",
		TargetNodeID:      "node-b",
		TransferKeyStatus: TransferKeyPending,
		CreatedAt:         100,
		UpdatedAt:         100,
	}
	failedFork := func(id, forkID string, createdAt int64) TransferSet {
		ts := base
		ts.ID = id
		ts.Kind = TransferSetFork
		ts.SpawnID = forkID
		ts.ForkSpawnID = forkID
		ts.Status = TransferSetFailed
		ts.CreatedAt = createdAt
		ts.UpdatedAt = createdAt
		return ts
	}
	create(failedFork("ts-fork-b", "sp-fork-b", 20))
	create(failedFork("ts-fork-a", "sp-fork-a", 10))
	activeFork := failedFork("ts-active-fork", "sp-fork-active", 30)
	activeFork.Status = TransferSetActive
	create(activeFork)
	migration := base
	migration.ID = "ts-failed-migration"
	migration.Kind = TransferSetMigration
	migration.SpawnID = "sp-migration"
	migration.Status = TransferSetFailed
	create(migration)
	create(failedFork("ts-deleted-fork", "sp-fork-deleted", 40))
	if err := st.Spawns().MarkDeleted(ctx, "sp-fork-deleted", 99); err != nil {
		t.Fatalf("MarkDeleted fork: %v", err)
	}

	rows, err := st.TransferSets().ListFailedForks(ctx)
	if err != nil {
		t.Fatalf("ListFailedForks: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListFailedForks rows = %+v, want 2 visible failed fork rows", rows)
	}
	if rows[0].ID != "ts-fork-a" || rows[1].ID != "ts-fork-b" {
		t.Fatalf("ListFailedForks order/ids = %+v, want ts-fork-a, ts-fork-b", rows)
	}
	for _, row := range rows {
		if row.Kind != TransferSetFork || row.Status != TransferSetFailed {
			t.Fatalf("ListFailedForks returned non-failed-fork row: %+v", row)
		}
	}
}

func TestListReclaimableForksIncludesFailedAndStaleRestoring(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	for _, id := range []string{
		"sp-source",
		"sp-fork-failed",
		"sp-fork-stale-restoring",
		"sp-fork-fresh-restoring",
		"sp-fork-deleted",
	} {
		seedTransferSetSpawn(t, st, id, "alice")
	}

	create := func(ts TransferSet) {
		t.Helper()
		if err := st.TransferSets().Create(ctx, ts); err != nil {
			t.Fatalf("Create %s: %v", ts.ID, err)
		}
	}
	base := TransferSet{
		Kind:              TransferSetFork,
		SourceSpawnID:     "sp-source",
		SourceGeneration:  7,
		TargetGeneration:  1,
		SourceNodeID:      "node-a",
		TargetNodeID:      "node-b",
		TransferKeyStatus: TransferKeyPending,
		CreatedAt:         100,
	}
	fork := func(id, forkID string, status TransferSetStatus, updatedAt int64) TransferSet {
		ts := base
		ts.ID = id
		ts.SpawnID = forkID
		ts.ForkSpawnID = forkID
		ts.Status = status
		ts.UpdatedAt = updatedAt
		return ts
	}
	create(fork("ts-failed", "sp-fork-failed", TransferSetFailed, 500))
	create(fork("ts-stale-restoring", "sp-fork-stale-restoring", TransferSetRestoring, 100))
	create(fork("ts-fresh-restoring", "sp-fork-fresh-restoring", TransferSetRestoring, 900))
	create(fork("ts-deleted-restoring", "sp-fork-deleted", TransferSetRestoring, 100))
	if err := st.Spawns().MarkDeleted(ctx, "sp-fork-deleted", 99); err != nil {
		t.Fatalf("MarkDeleted fork: %v", err)
	}

	rows, err := st.TransferSets().ListReclaimableForks(ctx, 700)
	if err != nil {
		t.Fatalf("ListReclaimableForks: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListReclaimableForks rows = %+v, want failed + stale restoring", rows)
	}
	if rows[0].ID != "ts-failed" || rows[1].ID != "ts-stale-restoring" {
		t.Fatalf("ListReclaimableForks order/ids = %+v, want ts-failed, ts-stale-restoring", rows)
	}
}
