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

// makeBundleWithMemberForKS creates a skill_bundle + a single version whose sole member is
// catalogID, and returns the new versionID.
func makeBundleWithMemberForKS(t *testing.T, s *Server, bundleID, versionID, catalogID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.st.SkillBundles().Create(ctx, store.SkillBundle{
		BundleID: bundleID, CreatorID: "alice", Name: bundleID, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("makeBundleWithMemberForKS: SkillBundles().Create: %v", err)
	}
	if err := s.st.SkillBundles().CreateVersion(ctx,
		store.SkillBundleVersion{VersionID: versionID, BundleID: bundleID, Seq: 1, CreatedAt: 1},
		[]store.SkillBundleMember{{SourceSubdir: "skill-a", CatalogID: catalogID, Position: 0}},
	); err != nil {
		t.Fatalf("makeBundleWithMemberForKS: CreateVersion: %v", err)
	}
}

// addBundleRefEntryForKS adds a bundle_ref ProfileEntry (pinned to bundleID/versionID) to an
// existing profile via the profile store directly.
func addBundleRefEntryForKS(t *testing.T, s *Server, profileID, entryID, bundleID, versionID string) {
	t.Helper()
	ctx := context.Background()
	p, _, _, err := s.st.Profiles().Get(ctx, profileID)
	if err != nil {
		t.Fatalf("addBundleRefEntryForKS: get profile %s: %v", profileID, err)
	}
	if _, err := s.st.Profiles().AddEntry(ctx, profileID, p.Version, store.ProfileEntry{
		EntryID:    entryID,
		Kind:       store.ProfileEntrySkill,
		Name:       "kill-switch-bundle-entry",
		SourceKind: store.ProfileSourceBundle,
		BundleID:   bundleID,
		VersionID:  versionID,
	}, 2000); err != nil {
		t.Fatalf("addBundleRefEntryForKS: %v", err)
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

	// Alice deletes the catalog entry — it's referenced, so this requires force=true
	// (sp-mwco.3.3 §4.3); the kill-switch must still terminate the spawn.
	if _, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
		CatalogId: catID, Force: true,
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

	// Alice deletes the entry (force=true — it's referenced by bob's profile) → bob's spawn must
	// be terminated.
	if _, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
		CatalogId: catID, Force: true,
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

	// Referenced by pfID (both the dead and the live spawn's profile) — force=true.
	if _, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
		CatalogId: catID, Force: true,
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

	// Delist — kill-switch fires. Confirm:true since the entry is referenced (§4.6 D5 guard).
	if _, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID,
		Listed:    false,
		Confirm:   true,
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
	s.SetAdminOwners([]string{"admin"}) // relisting requires admin (§4.6 D4)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "alice")
	addCatalogRefEntryForKS(t, s, pfID, "entry-1", catID)

	spawnID := "sp-relist-ks"
	makeSpawnForKS(t, s, spawnID, "alice", pfID)

	// First delist (creator, confirmed — the entry is referenced) to make it not-listed, then
	// relist as admin — relist must NOT kill.
	if _, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID,
		Listed:    false,
		Confirm:   true,
	})); err != nil {
		t.Fatalf("SetCatalogListing false: %v", err)
	}

	// The spawn was just terminated by the delist. Create a fresh one.
	spawnID2 := "sp-relist-ks2"
	makeSpawnForKS(t, s, spawnID2, "alice", pfID)

	// Relist (admin) — must NOT kill sp-relist-ks2.
	if _, err := s.SetCatalogListing(adminCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID,
		Listed:    true,
	})); err != nil {
		t.Fatalf("SetCatalogListing true: %v", err)
	}

	if isDeleted(t, s, spawnID2) {
		t.Errorf("spawn %s should NOT be killed by SetCatalogListing(listed=true)", spawnID2)
	}
}

// --- Kill-switch tests for bundle_ref membership (sp-mwco.1.6) -------------------

// TestDeleteBundleVersion_KillSwitch_BundleRef_TerminatesAffectedSpawn verifies that deleting
// the bundle version containing a catalog entry terminates a live spawn whose profile
// references it only through a bundle_ref entry pinned to that version (no catalog_ref at
// all). sp-mwco.3.3 makes a bundle-member catalog entry undeletable via DeleteCatalogEntry
// (force does not override bundle membership — the caller must delete the bundle version
// instead), so this exercises the kill-switch through DeleteBundleVersion rather than
// DeleteCatalogEntry.
func TestDeleteBundleVersion_KillSwitch_BundleRef_TerminatesAffectedSpawn(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "bundled-skill")
	makeBundleWithMemberForKS(t, s, "bnd-1", "bndv-1", catID)

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "bob")
	addBundleRefEntryForKS(t, s, pfID, "entry-1", "bnd-1", "bndv-1")

	spawnID := "sp-ks-bundle-1"
	makeSpawnForKS(t, s, spawnID, "bob", pfID)

	// Referenced by pfID's bundle_ref entry, so force=true (sp-mwco.3.3 §4.3).
	if _, err := s.DeleteBundleVersion(aliceCtx(), connect.NewRequest(&cpv1.DeleteBundleVersionRequest{
		VersionId: "bndv-1", Force: true,
	})); err != nil {
		t.Fatalf("DeleteBundleVersion: %v", err)
	}

	if !isDeleted(t, s, spawnID) {
		t.Errorf("spawn %s should be deleted after revoking a bundle_ref member's bundle version", spawnID)
	}
}

// TestSetCatalogListing_KillSwitch_BundleRef_DelistTerminates verifies that delisting a
// catalog entry that is a bundle member (with ZERO catalog_ref profiles — the sp-mwco.3.4
// resolve-gate defect) terminates the affected bundle_ref spawn.
func TestSetCatalogListing_KillSwitch_BundleRef_DelistTerminates(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "bundled-skill")
	makeBundleWithMemberForKS(t, s, "bnd-2", "bndv-2", catID)

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "bob")
	addBundleRefEntryForKS(t, s, pfID, "entry-1", "bnd-2", "bndv-2")

	spawnID := "sp-ks-bundle-delist"
	makeSpawnForKS(t, s, spawnID, "bob", pfID)

	// No catalog_ref profiles reference catID (profiles==0), only the bundle membership — the
	// resolve gate must still fire.
	if _, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID,
		Listed:    false,
		Confirm:   true,
	})); err != nil {
		t.Fatalf("SetCatalogListing false: %v", err)
	}

	if !isDeleted(t, s, spawnID) {
		t.Errorf("spawn %s should be deleted after delisting a bundle_ref member's catalog entry", spawnID)
	}
}

// TestDeleteBundleVersion_KillSwitch_OnlyAffectsBundleRefLeg verifies that deleting a bundle
// version terminates the spawn reached through its bundle_ref leg, and leaves a spawn reached
// through a separate catalog_ref to the very same catalog entry untouched.
//
// This supersedes the pre-sp-mwco.3.3 "union" test: that test deleted the shared catalog entry
// directly and expected BOTH legs to terminate in one call. Under sp-mwco.3.3's safe-delete
// semantics a catalog entry that is a bundle member can never be deleted via DeleteCatalogEntry
// (force does not override bundle membership), so the two legs can no longer be revoked by a
// single call — deleting the bundle version is scoped to that version's bundle_ref profiles only.
func TestDeleteBundleVersion_KillSwitch_OnlyAffectsBundleRefLeg(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "shared-skill")
	makeBundleWithMemberForKS(t, s, "bnd-3", "bndv-3", catID)

	// Profile referencing catID directly via catalog_ref.
	pfDirect := uuid.NewString()
	createProfileForKS(t, s, pfDirect, "bob")
	addCatalogRefEntryForKS(t, s, pfDirect, "entry-direct", catID)
	spawnDirect := "sp-ks-union-direct"
	makeSpawnForKS(t, s, spawnDirect, "bob", pfDirect)

	// Different profile referencing catID only via bundle_ref.
	pfBundle := uuid.NewString()
	createProfileForKS(t, s, pfBundle, "carol")
	addBundleRefEntryForKS(t, s, pfBundle, "entry-bundle", "bnd-3", "bndv-3")
	spawnBundle := "sp-ks-union-bundle"
	makeSpawnForKS(t, s, spawnBundle, "carol", pfBundle)

	// Referenced by pfBundle's bundle_ref entry, so force=true (sp-mwco.3.3 §4.3).
	if _, err := s.DeleteBundleVersion(aliceCtx(), connect.NewRequest(&cpv1.DeleteBundleVersionRequest{
		VersionId: "bndv-3", Force: true,
	})); err != nil {
		t.Fatalf("DeleteBundleVersion: %v", err)
	}

	if isDeleted(t, s, spawnDirect) {
		t.Errorf("catalog_ref spawn %s must NOT be deleted by a bundle version delete", spawnDirect)
	}
	if !isDeleted(t, s, spawnBundle) {
		t.Errorf("bundle_ref spawn %s should be deleted", spawnBundle)
	}
}

// TestDeleteBundleVersion_KillSwitch_DroppedVersionNotAffected verifies that a profile pinned
// to a bundle version that does NOT contain the revoked entry (e.g. the member was dropped in
// a later version) is NOT terminated when the OLD version (the one still containing the entry)
// is deleted.
func TestDeleteBundleVersion_KillSwitch_DroppedVersionNotAffected(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "dropped-skill")
	// bndv-old contains catID as a member; bndv-new does not.
	makeBundleWithMemberForKS(t, s, "bnd-4", "bndv-old", catID)
	if err := s.st.SkillBundles().CreateVersion(context.Background(),
		store.SkillBundleVersion{VersionID: "bndv-new", BundleID: "bnd-4", Seq: 2, CreatedAt: 2},
		[]store.SkillBundleMember{{SourceSubdir: "skill-b", CatalogID: "some-other-catalog-id", Position: 0}},
	); err != nil {
		t.Fatalf("CreateVersion bndv-new: %v", err)
	}

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "bob")
	// Profile is pinned to the NEW version, which dropped catID.
	addBundleRefEntryForKS(t, s, pfID, "entry-1", "bnd-4", "bndv-new")

	spawnID := "sp-ks-dropped"
	makeSpawnForKS(t, s, spawnID, "bob", pfID)

	// bndv-old has no bundle_ref profiles pinned to it (pfID is pinned to bndv-new), so no force
	// needed.
	if _, err := s.DeleteBundleVersion(aliceCtx(), connect.NewRequest(&cpv1.DeleteBundleVersionRequest{
		VersionId: "bndv-old",
	})); err != nil {
		t.Fatalf("DeleteBundleVersion: %v", err)
	}

	if isDeleted(t, s, spawnID) {
		t.Errorf("spawn %s pinned to a version that dropped the member must NOT be terminated", spawnID)
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
