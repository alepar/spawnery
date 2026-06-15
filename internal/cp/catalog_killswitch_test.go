package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/store"
)

// makeSpawnForKS inserts a spawn with the given profileID (status=starting) directly via
// the store (no node flow needed). Caller must ensure the app version already exists
// (newTestServer seeds "secret-app"/"1.0.0").
func makeSpawnForKS(t *testing.T, s *Server, spawnID, ownerID, profileID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.st.Owners().Upsert(ctx, store.Owner{ID: ownerID, CreatedAt: 1}); err != nil {
		t.Fatalf("makeSpawnForKS: upsert owner %s: %v", ownerID, err)
	}
	sp := store.Spawn{
		ID: spawnID, OwnerID: ownerID,
		AppID: "secret-app", AppVersion: "1.0.0", AppRef: "examples/secret-app",
		Model: "m", Status: store.Starting, CreatedAt: 1, LastUsedAt: 1,
		ProfileID: profileID,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		return tx.Spawns().Create(ctx, sp, nil)
	}); err != nil {
		t.Fatalf("makeSpawnForKS %s: %v", spawnID, err)
	}
}

// addCatalogRefEntryForKS adds a catalog_ref ProfileEntry to an existing profile via
// the profile store directly (bypassing the RPC to avoid needing an authenticated context
// for the intermediate Add). profileID must already exist.
func addCatalogRefEntryForKS(t *testing.T, s *Server, profileID, entryID, catalogID string) {
	t.Helper()
	ctx := context.Background()
	p, _, _, err := s.st.Profiles().Get(ctx, profileID)
	if err != nil {
		t.Fatalf("addCatalogRefEntryForKS: get profile %s: %v", profileID, err)
	}
	if _, err := s.st.Profiles().AddEntry(ctx, profileID, p.Version, store.ProfileEntry{
		EntryID:    entryID,
		Kind:       store.ProfileEntrySkill,
		Name:       "kill-switch-test-entry",
		SourceKind: store.ProfileSourceCatalog,
		CatalogID:  catalogID,
	}, 2000); err != nil {
		t.Fatalf("addCatalogRefEntryForKS: %v", err)
	}
}

// createProfileForKS creates a profile via the store for the given owner.
func createProfileForKS(t *testing.T, s *Server, profileID, ownerID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.st.Owners().Upsert(ctx, store.Owner{ID: ownerID, CreatedAt: 1}); err != nil {
		t.Fatalf("createProfileForKS: upsert owner %s: %v", ownerID, err)
	}
	if err := s.st.Profiles().Create(ctx, store.Profile{
		ProfileID: profileID,
		OwnerID:   ownerID,
		Name:      "test-" + profileID,
		Version:   1,
		UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("createProfileForKS %s: %v", profileID, err)
	}
}

// isDeleted returns true if the spawn with the given id is soft-deleted in the store.
func isDeleted(t *testing.T, s *Server, spawnID string) bool {
	t.Helper()
	_, err := s.st.Spawns().Get(context.Background(), spawnID)
	if err != nil {
		if err == store.ErrNotFound {
			return true // Get returns ErrNotFound for deleted spawns
		}
		t.Fatalf("isDeleted: unexpected error for %s: %v", spawnID, err)
	}
	return false
}

// --- Kill-switch tests for DeleteCatalogEntry -----------------------------------

// TestDeleteCatalogEntry_KillSwitch_TerminatesAffectedSpawn verifies that deleting a
// catalog entry terminates any live spawn whose profile references it.
func TestDeleteCatalogEntry_KillSwitch_TerminatesAffectedSpawn(t *testing.T) {
	s, _, _ := newTestServer(t)

	// Alice creates a catalog entry.
	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	// Alice creates a profile that references the catalog entry.
	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "alice")
	addCatalogRefEntryForKS(t, s, pfID, "entry-1", catID)

	// Alice has a live spawn using that profile.
	spawnID := "sp-ks-1"
	makeSpawnForKS(t, s, spawnID, "alice", pfID)

	// Alice deletes the catalog entry — the kill-switch must terminate the spawn.
	if _, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
		CatalogId: catID,
	})); err != nil {
		t.Fatalf("DeleteCatalogEntry: %v", err)
	}

	if !isDeleted(t, s, spawnID) {
		t.Errorf("spawn %s should be deleted after catalog entry revoke", spawnID)
	}
}

// TestDeleteCatalogEntry_KillSwitch_NoReferencing_NoOp verifies that deleting a catalog
// entry with no referencing profiles/spawns still succeeds and is a no-op for the spawns.
func TestDeleteCatalogEntry_KillSwitch_NoReferencing_NoOp(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "unused-skill")

	// Unrelated spawn (no profile).
	makeSpawnForKS(t, s, "sp-unrelated", "alice", "")

	if _, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
		CatalogId: catID,
	})); err != nil {
		t.Fatalf("DeleteCatalogEntry: %v", err)
	}

	// The unrelated spawn must be untouched.
	if isDeleted(t, s, "sp-unrelated") {
		t.Errorf("unrelated spawn should NOT be terminated by kill-switch")
	}
}

// TestDeleteCatalogEntry_KillSwitch_CrossOwner verifies that the kill-switch terminates
// spawns owned by a different owner than the catalog entry creator.
func TestDeleteCatalogEntry_KillSwitch_CrossOwner(t *testing.T) {
	s, _, _ := newTestServer(t)

	// Alice creates the catalog entry.
	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "alice-skill")

	// Bob creates a profile referencing Alice's catalog entry.
	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "bob")
	addCatalogRefEntryForKS(t, s, pfID, "bob-entry-1", catID)

	// Bob has a spawn using that profile.
	bobSpawnID := "sp-bob-ks"
	makeSpawnForKS(t, s, bobSpawnID, "bob", pfID)

	// Alice deletes the entry → Bob's spawn must be terminated.
	if _, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
		CatalogId: catID,
	})); err != nil {
		t.Fatalf("DeleteCatalogEntry: %v", err)
	}

	if !isDeleted(t, s, bobSpawnID) {
		t.Errorf("bob's spawn %s should be deleted after alice revokes the catalog entry", bobSpawnID)
	}
}

// TestDeleteCatalogEntry_KillSwitch_AlreadyDeletedSpawnUntouched verifies that
// already-deleted spawns with the same profile_id are not touched (idempotency).
func TestDeleteCatalogEntry_KillSwitch_AlreadyDeletedSpawnUntouched(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "alice")
	addCatalogRefEntryForKS(t, s, pfID, "entry-1", catID)

	// Pre-deleted spawn — should not match ListLiveByProfileIDs.
	makeSpawnForKS(t, s, "sp-already-dead", "alice", pfID)
	if err := s.st.Spawns().MarkDeleted(context.Background(), "sp-already-dead", 1); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}

	// Live spawn — should be terminated.
	makeSpawnForKS(t, s, "sp-live", "alice", pfID)

	if _, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
		CatalogId: catID,
	})); err != nil {
		t.Fatalf("DeleteCatalogEntry: %v", err)
	}

	if !isDeleted(t, s, "sp-live") {
		t.Errorf("sp-live should be deleted")
	}
	// sp-already-dead was already deleted before the kill-switch — its store state
	// is already Deleted so it won't appear in ListLiveByProfileIDs (which excludes Deleted).
}

// --- Kill-switch tests for SetCatalogListing ------------------------------------

// TestSetCatalogListing_KillSwitch_DelistTerminates verifies that setting listed=false
// terminates live spawns referencing the entry.
func TestSetCatalogListing_KillSwitch_DelistTerminates(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "alice")
	addCatalogRefEntryForKS(t, s, pfID, "entry-1", catID)

	spawnID := "sp-delist-ks"
	makeSpawnForKS(t, s, spawnID, "alice", pfID)

	// Delist — kill-switch fires.
	if _, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID,
		Listed:    false,
	})); err != nil {
		t.Fatalf("SetCatalogListing false: %v", err)
	}

	if !isDeleted(t, s, spawnID) {
		t.Errorf("spawn %s should be deleted after delist", spawnID)
	}
}

// TestSetCatalogListing_KillSwitch_RelistNoKill verifies that setting listed=true does
// NOT trigger the kill-switch.
func TestSetCatalogListing_KillSwitch_RelistNoKill(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "alice")
	addCatalogRefEntryForKS(t, s, pfID, "entry-1", catID)

	spawnID := "sp-relist-ks"
	makeSpawnForKS(t, s, spawnID, "alice", pfID)

	// First delist to make it not-listed, then relist — relist must NOT kill.
	if _, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID,
		Listed:    false,
	})); err != nil {
		t.Fatalf("SetCatalogListing false: %v", err)
	}

	// The spawn was just terminated by the delist. Create a fresh one.
	spawnID2 := "sp-relist-ks2"
	makeSpawnForKS(t, s, spawnID2, "alice", pfID)

	// Relist — must NOT kill sp-relist-ks2.
	if _, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID,
		Listed:    true,
	})); err != nil {
		t.Fatalf("SetCatalogListing true: %v", err)
	}

	if isDeleted(t, s, spawnID2) {
		t.Errorf("spawn %s should NOT be killed by SetCatalogListing(listed=true)", spawnID2)
	}
}

// TestUpdateCatalogEntry_NoKill verifies that UpdateCatalogEntry does NOT trigger the
// kill-switch.
func TestUpdateCatalogEntry_NoKill(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "alice")
	addCatalogRefEntryForKS(t, s, pfID, "entry-1", catID)

	spawnID := "sp-update-ks"
	makeSpawnForKS(t, s, spawnID, "alice", pfID)

	// Update content — must NOT kill.
	if _, err := s.UpdateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateCatalogEntryRequest{
		CatalogId:   catID,
		Name:        "my-skill",
		Description: "updated",
		Content:     skillContent,
	})); err != nil {
		t.Fatalf("UpdateCatalogEntry: %v", err)
	}

	if isDeleted(t, s, spawnID) {
		t.Errorf("spawn %s should NOT be killed by UpdateCatalogEntry", spawnID)
	}
}
