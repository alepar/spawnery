package store_test

import (
	"context"
	"testing"

	"spawnery/internal/cp/store"
)

// ----- helpers ----------------------------------------------------------------

// makeProfile inserts a minimal profile into st and returns its profile_id.
func makeProfile(t *testing.T, st store.Store, profileID, ownerID string) {
	t.Helper()
	ctx := context.Background()
	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: profileID,
		OwnerID:   ownerID,
		Name:      "test-profile-" + profileID,
		Version:   1,
		UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("makeProfile %s: %v", profileID, err)
	}
}

// addCatalogRefEntry adds a catalog_ref ProfileEntry to an existing profile.
func addCatalogRefEntry(t *testing.T, st store.Store, profileID, entryID, catalogID string) {
	t.Helper()
	ctx := context.Background()
	// Read current version.
	p, _, _, err := st.Profiles().Get(ctx, profileID)
	if err != nil {
		t.Fatalf("addCatalogRefEntry: get profile: %v", err)
	}
	_, err = st.Profiles().AddEntry(ctx, profileID, p.Version, store.ProfileEntry{
		EntryID:    entryID,
		Kind:       store.ProfileEntrySkill,
		Name:       "test-entry-" + entryID,
		SourceKind: store.ProfileSourceCatalog,
		CatalogID:  catalogID,
	}, 1001)
	if err != nil {
		t.Fatalf("addCatalogRefEntry %s/%s: %v", profileID, entryID, err)
	}
}

// addCustomEntry adds a custom (non-catalog_ref) ProfileEntry to an existing profile.
func addCustomEntry(t *testing.T, st store.Store, profileID, entryID string) {
	t.Helper()
	ctx := context.Background()
	p, _, _, err := st.Profiles().Get(ctx, profileID)
	if err != nil {
		t.Fatalf("addCustomEntry: get profile: %v", err)
	}
	_, err = st.Profiles().AddEntry(ctx, profileID, p.Version, store.ProfileEntry{
		EntryID:      entryID,
		Kind:         store.ProfileEntrySkill,
		Name:         "custom-" + entryID,
		SourceKind:   store.ProfileSourceCustom,
		CatalogID:    "",
		CustomInline: []byte("inline content"),
	}, 1001)
	if err != nil {
		t.Fatalf("addCustomEntry %s/%s: %v", profileID, entryID, err)
	}
}

// makeAppForKillswitch ensures the test app+version exists (re-uses secret-app from seed if DSN
// matches, but uses a dedicated in-memory store so we must insert ourselves).
func makeAppForKillswitch(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	if err := st.Apps().Upsert(ctx, store.App{
		ID:          "test-app",
		DisplayName: "Test App",
		Summary:     "unit test",
		Tags:        "",
		Visibility:  "public",
		Listed:      true,
		CreatorID:   "test-owner",
		CreatedAt:   1,
	}); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if err := st.Apps().UpsertVersion(ctx, store.AppVersion{
		AppID:     "test-app",
		Version:   "1.0",
		Ref:       "test/test-app",
		Tier:      store.TierReviewed,
		Manifest:  "{}",
		CreatedAt: 1,
	}, nil); err != nil {
		t.Fatalf("upsert app version: %v", err)
	}
}

// makeSpawnWithProfile inserts a spawn row with the given profile_id (status=starting).
func makeSpawnWithProfile(t *testing.T, st store.Store, spawnID, ownerID, profileID string) {
	t.Helper()
	ctx := context.Background()
	if err := st.Owners().Upsert(ctx, store.Owner{ID: ownerID, CreatedAt: 1}); err != nil {
		t.Fatalf("upsert owner %s: %v", ownerID, err)
	}
	sp := store.Spawn{
		ID: spawnID, OwnerID: ownerID, AppID: "test-app", AppVersion: "1.0", AppRef: "test/test-app",
		Model: "m", Status: store.Starting, CreatedAt: 1, LastUsedAt: 1, ProfileID: profileID,
	}
	if err := st.WithTx(ctx, func(tx store.Store) error {
		return tx.Spawns().Create(ctx, sp, nil)
	}); err != nil {
		t.Fatalf("makeSpawnWithProfile %s: %v", spawnID, err)
	}
}

// ----- ListProfileIDsByCatalogRef tests ----------------------------------------

func TestListProfileIDsByCatalogRef_Empty(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	ids, err := st.Profiles().ListProfileIDsByCatalogRef(ctx, "no-such-catalog")
	if err != nil {
		t.Fatalf("ListProfileIDsByCatalogRef: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty, got %v", ids)
	}
}

func TestListProfileIDsByCatalogRef_MatchesCatalogRefOnly(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	makeProfile(t, st, "pf-a", "owner1")
	// Add a catalog_ref entry pointing to "cat-1".
	addCatalogRefEntry(t, st, "pf-a", "entry-1", "cat-1")
	// Also add a custom entry — should not affect results.
	addCustomEntry(t, st, "pf-a", "entry-2")

	// Profile pf-b has a catalog_ref but to a DIFFERENT catalog.
	makeProfile(t, st, "pf-b", "owner2")
	addCatalogRefEntry(t, st, "pf-b", "entry-3", "cat-2")

	ids, err := st.Profiles().ListProfileIDsByCatalogRef(ctx, "cat-1")
	if err != nil {
		t.Fatalf("ListProfileIDsByCatalogRef cat-1: %v", err)
	}
	if len(ids) != 1 || ids[0] != "pf-a" {
		t.Errorf("expected [pf-a], got %v", ids)
	}

	// cat-2 should return pf-b only.
	ids2, err := st.Profiles().ListProfileIDsByCatalogRef(ctx, "cat-2")
	if err != nil {
		t.Fatalf("ListProfileIDsByCatalogRef cat-2: %v", err)
	}
	if len(ids2) != 1 || ids2[0] != "pf-b" {
		t.Errorf("expected [pf-b], got %v", ids2)
	}
}

func TestListProfileIDsByCatalogRef_Deduplication(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	// Two different entries in the SAME profile both referencing the same catalog_id.
	makeProfile(t, st, "pf-dup", "owner1")
	addCatalogRefEntry(t, st, "pf-dup", "entry-x", "cat-shared")
	addCatalogRefEntry(t, st, "pf-dup", "entry-y", "cat-shared")

	ids, err := st.Profiles().ListProfileIDsByCatalogRef(ctx, "cat-shared")
	if err != nil {
		t.Fatalf("ListProfileIDsByCatalogRef: %v", err)
	}
	if len(ids) != 1 || ids[0] != "pf-dup" {
		t.Errorf("expected exactly [pf-dup], got %v", ids)
	}
}

func TestListProfileIDsByCatalogRef_MultipleProfiles(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	// Multiple profiles all referencing the same catalog entry.
	for _, pid := range []string{"pf-1", "pf-2", "pf-3"} {
		makeProfile(t, st, pid, "owner1")
		addCatalogRefEntry(t, st, pid, "entry-"+pid, "cat-popular")
	}
	// Unrelated profile.
	makeProfile(t, st, "pf-other", "owner2")
	addCatalogRefEntry(t, st, "pf-other", "entry-other", "cat-other")

	ids, err := st.Profiles().ListProfileIDsByCatalogRef(ctx, "cat-popular")
	if err != nil {
		t.Fatalf("ListProfileIDsByCatalogRef: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 profile ids, got %d: %v", len(ids), ids)
	}
	byID := map[string]bool{}
	for _, id := range ids {
		byID[id] = true
	}
	for _, want := range []string{"pf-1", "pf-2", "pf-3"} {
		if !byID[want] {
			t.Errorf("expected %s in results, got %v", want, ids)
		}
	}
}

// ----- ListLiveByProfileIDs tests -----------------------------------------------

func TestListLiveByProfileIDs_EmptyInput(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	spawns, err := st.Spawns().ListLiveByProfileIDs(ctx, nil)
	if err != nil {
		t.Fatalf("ListLiveByProfileIDs nil: %v", err)
	}
	if spawns != nil {
		t.Errorf("expected nil for empty input, got %v", spawns)
	}

	spawns, err = st.Spawns().ListLiveByProfileIDs(ctx, []string{})
	if err != nil {
		t.Fatalf("ListLiveByProfileIDs empty: %v", err)
	}
	if spawns != nil {
		t.Errorf("expected nil for empty slice, got %v", spawns)
	}
}

func TestListLiveByProfileIDs_FiltersDeleted(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	makeAppForKillswitch(t, st)

	// Live spawn with profile pf-live.
	makeSpawnWithProfile(t, st, "sp-live", "owner1", "pf-live")
	// Deleted spawn with same profile — must NOT appear.
	makeSpawnWithProfile(t, st, "sp-dead", "owner1", "pf-live")
	if err := st.Spawns().MarkDeleted(ctx, "sp-dead", 999); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}

	spawns, err := st.Spawns().ListLiveByProfileIDs(ctx, []string{"pf-live"})
	if err != nil {
		t.Fatalf("ListLiveByProfileIDs: %v", err)
	}
	if len(spawns) != 1 || spawns[0].ID != "sp-live" {
		t.Errorf("expected [sp-live], got %v", spawns)
	}
}

func TestListLiveByProfileIDs_IncludesAllLiveStatuses(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	makeAppForKillswitch(t, st)

	// Starting spawn.
	makeSpawnWithProfile(t, st, "sp-starting", "owner1", "pf-x")
	// Active spawn: transition via the store.
	makeSpawnWithProfile(t, st, "sp-active", "owner1", "pf-x")
	if err := st.Spawns().SetActive(ctx, "sp-active", "node1", 1); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	spawns, err := st.Spawns().ListLiveByProfileIDs(ctx, []string{"pf-x"})
	if err != nil {
		t.Fatalf("ListLiveByProfileIDs: %v", err)
	}
	if len(spawns) != 2 {
		t.Errorf("expected 2 spawns (starting+active), got %d: %v", len(spawns), spawns)
	}
}

func TestListLiveByProfileIDs_MultiProfileINSet(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	makeAppForKillswitch(t, st)

	makeSpawnWithProfile(t, st, "sp-a", "owner1", "pf-a")
	makeSpawnWithProfile(t, st, "sp-b", "owner2", "pf-b")
	makeSpawnWithProfile(t, st, "sp-c", "owner3", "pf-c") // not in query

	spawns, err := st.Spawns().ListLiveByProfileIDs(ctx, []string{"pf-a", "pf-b"})
	if err != nil {
		t.Fatalf("ListLiveByProfileIDs: %v", err)
	}
	if len(spawns) != 2 {
		t.Errorf("expected 2, got %d: %v", len(spawns), spawns)
	}
	ids := map[string]bool{spawns[0].ID: true, spawns[1].ID: true}
	if !ids["sp-a"] || !ids["sp-b"] {
		t.Errorf("expected sp-a and sp-b, got %v", spawns)
	}
}

func TestListLiveByProfileIDs_OrderedByID(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	makeAppForKillswitch(t, st)

	// Insert in reverse order; result should be ASC.
	for _, id := range []string{"sp-z", "sp-a", "sp-m"} {
		makeSpawnWithProfile(t, st, id, "owner1", "pf-order")
	}

	spawns, err := st.Spawns().ListLiveByProfileIDs(ctx, []string{"pf-order"})
	if err != nil {
		t.Fatalf("ListLiveByProfileIDs: %v", err)
	}
	if len(spawns) != 3 {
		t.Fatalf("expected 3, got %d", len(spawns))
	}
	want := []string{"sp-a", "sp-m", "sp-z"}
	for i, w := range want {
		if spawns[i].ID != w {
			t.Errorf("spawn[%d]: got %q, want %q", i, spawns[i].ID, w)
		}
	}
}

func TestListLiveByProfileIDs_EmptyProfileID_NotMatched(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	makeAppForKillswitch(t, st)

	// Spawn with no profile (empty string).
	makeSpawnWithProfile(t, st, "sp-no-profile", "owner1", "")

	// Querying with a non-empty profile id must not match the profileless spawn.
	spawns, err := st.Spawns().ListLiveByProfileIDs(ctx, []string{"pf-nonexistent"})
	if err != nil {
		t.Fatalf("ListLiveByProfileIDs: %v", err)
	}
	if len(spawns) != 0 {
		t.Errorf("expected empty, got %v", spawns)
	}
}
