package store

import (
	"context"
	"testing"
)

func TestSpawnCreatePersistsForkLineage(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	source := newSpawn("sp-source")
	source.Name = "source"
	inTx(t, st, func(tx Store) error {
		return tx.Spawns().Create(ctx, source, []Mount{{Name: "main", BackendURI: "scratch"}})
	})

	forkedAt := int64(1234)
	parentID := "sp-source"
	child := newSpawn("sp-fork")
	child.Name = "source fork"
	child.ParentSpawnID = &parentID
	child.ForkedAt = &forkedAt
	inTx(t, st, func(tx Store) error {
		return tx.Spawns().Create(ctx, child, []Mount{{Name: "main", BackendURI: "scratch"}})
	})

	got, err := st.Spawns().Get(ctx, "sp-fork")
	if err != nil {
		t.Fatalf("Get fork: %v", err)
	}
	if got.ParentSpawnID == nil || *got.ParentSpawnID != parentID {
		t.Fatalf("ParentSpawnID=%v want %q", got.ParentSpawnID, parentID)
	}
	if got.ForkedAt == nil || *got.ForkedAt != forkedAt {
		t.Fatalf("ForkedAt=%v want %d", got.ForkedAt, forkedAt)
	}

	rows, err := st.Spawns().ListByOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	var found bool
	for _, row := range rows {
		if row.ID == "sp-fork" {
			found = row.ParentSpawnID != nil && *row.ParentSpawnID == parentID &&
				row.ForkedAt != nil && *row.ForkedAt == forkedAt
		}
	}
	if !found {
		t.Fatalf("fork lineage missing from ListByOwner rows: %+v", rows)
	}
}

func TestSpawnLineageDoesNotAffectSourceDeletion(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	source := newSpawn("sp-source")
	inTx(t, st, func(tx Store) error {
		return tx.Spawns().Create(ctx, source, []Mount{{Name: "main", BackendURI: "scratch"}})
	})
	parentID := "sp-source"
	forkedAt := int64(1234)
	child := newSpawn("sp-fork")
	child.ParentSpawnID = &parentID
	child.ForkedAt = &forkedAt
	inTx(t, st, func(tx Store) error {
		return tx.Spawns().Create(ctx, child, []Mount{{Name: "main", BackendURI: "scratch"}})
	})

	if err := st.Spawns().MarkDeleted(ctx, "sp-source", 99); err != nil {
		t.Fatalf("MarkDeleted source: %v", err)
	}
	if _, err := st.Spawns().Get(ctx, "sp-source"); err == nil {
		t.Fatal("deleted source must be hidden from Get")
	}
	got, err := st.Spawns().Get(ctx, "sp-fork")
	if err != nil {
		t.Fatalf("fork must remain readable after source deletion: %v", err)
	}
	if got.ParentSpawnID == nil || *got.ParentSpawnID != parentID {
		t.Fatalf("fork lineage changed after source deletion: %+v", got)
	}
}
