package cp

// Handler-level tests for sp-mwco.3.3's safe delete: the counts-only reference-check refusal
// (and its cross-tenant-disclosure regression), the force override rules — including that force
// does NOT override a bundle-version membership — the WithTx/LockRow race fix between
// DeleteCatalogEntry and AddProfileEntry, and DeleteBundle/DeleteBundleVersion.

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
)

// --- DeleteCatalogEntry: reference check ----------------------------------------------------

// TestDeleteCatalogEntry_ReferencedByProfiles_FailedPrecondition proves the counts-only refusal:
// a referenced entry cannot be deleted without force, the message carries counts only, and it
// never leaks a profile id, owner id, or spawn id — the catalog is global and refs span tenants,
// so naming them would be a cross-tenant disclosure.
func TestDeleteCatalogEntry_ReferencedByProfiles_FailedPrecondition(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "widely-used")

	pf1 := "owner-SECRET-a-profile"
	createProfileForKS(t, s, pf1, "owner-SECRET-a")
	addCatalogRefEntryForKS(t, s, pf1, "e1", catID)

	pf2 := "owner-SECRET-b-profile"
	createProfileForKS(t, s, pf2, "owner-SECRET-b")
	addCatalogRefEntryForKS(t, s, pf2, "e2", catID)

	pf3 := "owner-SECRET-b-profile-2"
	createProfileForKS(t, s, pf3, "owner-SECRET-b")
	addCatalogRefEntryForKS(t, s, pf3, "e3", catID)

	_, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
		CatalogId: catID,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "3 profile") || !strings.Contains(msg, "2 owner") {
		t.Errorf("expected message to contain reference counts, got %q", msg)
	}
	for _, secret := range []string{pf1, pf2, pf3, "owner-SECRET-a", "owner-SECRET-b"} {
		if strings.Contains(msg, secret) {
			t.Errorf("error message leaks id %q: %q", secret, msg)
		}
	}

	// The entry must still exist — the refused delete must not have partially applied.
	if _, err := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID})); err != nil {
		t.Errorf("expected entry to survive a refused delete, got: %v", err)
	}
}

// TestDeleteCatalogEntry_Force_DeletesDespiteRefs proves force=true bypasses the profile-reference
// check: the catalog row is gone, the kill-switch fires for the referencing spawn, and the
// profile entry survives as a deliberate dangling ref (assembly fails loud on it — defense in
// depth, exercised elsewhere).
func TestDeleteCatalogEntry_Force_DeletesDespiteRefs(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "widely-used")

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "alice")
	addCatalogRefEntryForKS(t, s, pfID, "e1", catID)
	spawnID := "sp-force-delete"
	makeSpawnForKS(t, s, spawnID, "alice", pfID)

	if _, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
		CatalogId: catID, Force: true,
	})); err != nil {
		t.Fatalf("DeleteCatalogEntry(force=true): %v", err)
	}

	if _, err := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected the catalog row to be gone, got: %v", err)
	}
	if !isDeleted(t, s, spawnID) {
		t.Errorf("spawn %s should be terminated by the kill-switch on a forced delete", spawnID)
	}
	// The dangling ref itself survives — DeleteCatalogEntry has no FK to profile_entries.
	_, entries, _, err := s.st.Profiles().Get(context.Background(), pfID)
	if err != nil {
		t.Fatalf("Profiles().Get: %v", err)
	}
	if len(entries) != 1 || entries[0].CatalogID != catID {
		t.Errorf("expected the dangling catalog_ref entry to survive, got: %+v", entries)
	}
}

// --- DeleteCatalogEntry: bundle membership (force does NOT override) -----------------------

// TestDeleteCatalogEntry_BundleMember_NoForce_Rejected proves a catalog entry that is a bundle
// version member is rejected outright.
func TestDeleteCatalogEntry_BundleMember_NoForce_Rejected(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "bundle-member-skill")
	seedBundleWithMember(t, s, "bundle-1", "v1", catID)

	_, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{CatalogId: catID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "1 bundle version") || !strings.Contains(err.Error(), "delete the bundle version instead") {
		t.Errorf("expected a bundle-membership message, got %q", err.Error())
	}
	if _, err := s.st.CustomizationCatalog().Get(ctx, catID); err != nil {
		t.Errorf("expected the entry to survive, got: %v", err)
	}
}

// TestDeleteCatalogEntry_Force_BundleMember_StillRejected proves force does NOT override the
// bundle-membership check — forcing would orphan a live bundle version.
func TestDeleteCatalogEntry_Force_BundleMember_StillRejected(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "bundle-member-skill")
	seedBundleWithMember(t, s, "bundle-1", "v1", catID)

	_, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
		CatalogId: catID, Force: true,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition even with force=true, got %v", err)
	}
}

// seedBundleWithMember creates a bundle + one version with catalogID as its sole member.
func seedBundleWithMember(t *testing.T, s *Server, bundleID, versionID, catalogID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.st.SkillBundles().Create(ctx, store.SkillBundle{
		BundleID: bundleID, CreatorID: "alice", Name: "b", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("SkillBundles().Create: %v", err)
	}
	if err := s.st.SkillBundles().CreateVersion(ctx,
		store.SkillBundleVersion{VersionID: versionID, BundleID: bundleID, Seq: 1, CreatedAt: 1},
		[]store.SkillBundleMember{{SourceSubdir: "skills/a", CatalogID: catalogID, Position: 0}},
	); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
}

// addBundleRefEntryForTest adds a bundle_ref ProfileEntry directly via the store (bundle_ref
// entries have no wire-level attach RPC yet — sp-mwco.1's follow-on slice; see catalog_delete.go's
// doc comment). profileID must already exist.
func addBundleRefEntryForTest(t *testing.T, s *Server, profileID, entryID, bundleID, versionID string) {
	t.Helper()
	ctx := context.Background()
	p, _, _, err := s.st.Profiles().Get(ctx, profileID)
	if err != nil {
		t.Fatalf("addBundleRefEntryForTest: get profile %s: %v", profileID, err)
	}
	if _, err := s.st.Profiles().AddEntry(ctx, profileID, p.Version, store.ProfileEntry{
		EntryID:    entryID,
		Kind:       store.ProfileEntrySkill,
		Name:       "bundle-ref-test-entry",
		SourceKind: store.ProfileSourceBundle,
		BundleID:   bundleID,
		VersionID:  versionID,
	}, 2000); err != nil {
		t.Fatalf("addBundleRefEntryForTest: %v", err)
	}
}

// --- DeleteBundle -------------------------------------------------------------------------

func TestDeleteBundle_NotCreator_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedBundleWithMember(t, s, "bundle-1", "v1", "cat-x")

	_, err := s.DeleteBundle(bobCtx(), connect.NewRequest(&cpv1.DeleteBundleRequest{BundleId: "bundle-1"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", err)
	}
}

func TestDeleteBundle_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.DeleteBundle(aliceCtx(), connect.NewRequest(&cpv1.DeleteBundleRequest{BundleId: "no-such"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestDeleteBundle_ReferencedByProfiles_FailedPrecondition_CountsOnly(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedBundleWithMember(t, s, "bundle-1", "v1", "cat-x")

	pf1 := "owner-SECRET-profile"
	createProfileForKS(t, s, pf1, "owner-SECRET")
	addBundleRefEntryForTest(t, s, pf1, "e1", "bundle-1", "v1")

	_, err := s.DeleteBundle(aliceCtx(), connect.NewRequest(&cpv1.DeleteBundleRequest{BundleId: "bundle-1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "1 profile") || !strings.Contains(msg, "1 owner") {
		t.Errorf("expected reference counts in message, got %q", msg)
	}
	if strings.Contains(msg, pf1) || strings.Contains(msg, "owner-SECRET") {
		t.Errorf("error message leaks an id: %q", msg)
	}
}

func TestDeleteBundle_Force_DeletesDespiteRefs_KillSwitchFires(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedBundleWithMember(t, s, "bundle-1", "v1", "cat-x")

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "alice")
	addBundleRefEntryForTest(t, s, pfID, "e1", "bundle-1", "v1")
	spawnID := "sp-bundle-force"
	makeSpawnForKS(t, s, spawnID, "alice", pfID)

	if _, err := s.DeleteBundle(aliceCtx(), connect.NewRequest(&cpv1.DeleteBundleRequest{
		BundleId: "bundle-1", Force: true,
	})); err != nil {
		t.Fatalf("DeleteBundle(force=true): %v", err)
	}

	if _, err := s.st.SkillBundles().Get(context.Background(), "bundle-1"); err == nil {
		t.Error("expected bundle to be gone")
	}
	if !isDeleted(t, s, spawnID) {
		t.Errorf("spawn %s should be terminated by the kill-switch", spawnID)
	}
}

// --- DeleteBundleVersion -------------------------------------------------------------------

func TestDeleteBundleVersion_NotCreator_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedBundleWithMember(t, s, "bundle-1", "v1", "cat-x")

	_, err := s.DeleteBundleVersion(bobCtx(), connect.NewRequest(&cpv1.DeleteBundleVersionRequest{VersionId: "v1"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", err)
	}
}

func TestDeleteBundleVersion_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.DeleteBundleVersion(aliceCtx(), connect.NewRequest(&cpv1.DeleteBundleVersionRequest{VersionId: "no-such"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestDeleteBundleVersion_ReferencedByProfiles_FailedPrecondition(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedBundleWithMember(t, s, "bundle-1", "v1", "cat-x")

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "alice")
	addBundleRefEntryForTest(t, s, pfID, "e1", "bundle-1", "v1")

	_, err := s.DeleteBundleVersion(aliceCtx(), connect.NewRequest(&cpv1.DeleteBundleVersionRequest{VersionId: "v1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

// TestDeleteBundleVersion_Force_DeletesDespiteRefs_SiblingVersionIntact proves force deletes the
// targeted version while a sibling version (v2, unreferenced) of the same bundle is untouched.
func TestDeleteBundleVersion_Force_DeletesDespiteRefs_SiblingVersionIntact(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()
	seedBundleWithMember(t, s, "bundle-1", "v1", "cat-a")
	if err := s.st.SkillBundles().CreateVersion(ctx,
		store.SkillBundleVersion{VersionID: "v2", BundleID: "bundle-1", Seq: 2, CreatedAt: 1},
		nil,
	); err != nil {
		t.Fatalf("CreateVersion v2: %v", err)
	}

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "alice")
	addBundleRefEntryForTest(t, s, pfID, "e1", "bundle-1", "v1")

	if _, err := s.DeleteBundleVersion(aliceCtx(), connect.NewRequest(&cpv1.DeleteBundleVersionRequest{
		VersionId: "v1", Force: true,
	})); err != nil {
		t.Fatalf("DeleteBundleVersion(force=true): %v", err)
	}

	if _, err := s.st.SkillBundles().GetVersion(ctx, "v1"); err == nil {
		t.Error("expected v1 to be gone")
	}
	if _, err := s.st.SkillBundles().GetVersion(ctx, "v2"); err != nil {
		t.Errorf("expected sibling v2 to survive, got: %v", err)
	}
}

// --- The race: DeleteCatalogEntry vs. a concurrent AddProfileEntry -------------------------

// newLockRaceTestServer builds a Server over a store opened with the `_txlock=immediate` +
// busy_timeout DSN (sp-mwco.3.3 §2) instead of store.NewTestStore's default DSN, so two
// concurrent WithTx transactions serialize (block-then-proceed) rather than racing straight into
// SQLITE_BUSY. Mirrors newTestServerSink's setup (server_test.go).
//
// Backed by a real temp file, NOT `mode=memory&cache=shared`: shared-cache mode's per-table
// locking returns SQLITE_LOCKED_SHAREDCACHE on contention between connections, which
// busy_timeout's retry loop does not cover (see internal/cp/store/safe_delete_test.go's
// newLockTestStore for the full explanation).
func newLockRaceTestServer(t *testing.T, name string) *Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), name+".sqlite")
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	st, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open lock race test store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := registry.New()
	rt := router.New()
	sc := scheduler.New(reg, rt, time.Second)
	if err := Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]AppSeed{{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	return NewServer(reg, rt, sc, st, telemetry.NopSink{})
}

// TestDeleteCatalogEntry_ConcurrentAttach_NoDanglingRef is the load-bearing proof for sp-mwco.3.3
// §2: LockRow serializes DeleteCatalogEntry against a concurrent AddProfileEntry on the same
// catalog_id. We assert the invariant, not who wins:
//   - delete succeeded (row gone)         => zero profile entries reference the id (attach lost
//     the race and observed NotFound), and
//   - delete was refused (FailedPrecondition) => the catalog row still exists AND the attach
//     committed (that's exactly why the delete saw a nonzero ref count).
//
// Either way, no BUSY/Internal driver error is permitted from either side.
func TestDeleteCatalogEntry_ConcurrentAttach_NoDanglingRef(t *testing.T) {
	const n = 20
	for i := 0; i < n; i++ {
		s := newLockRaceTestServer(t, t.Name()+"-"+uuid.NewString())

		catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "race-skill")
		cr, err := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "P"}))
		if err != nil {
			t.Fatalf("iter %d: CreateProfile: %v", i, err)
		}
		profileID := cr.Msg.ProfileId

		var wg sync.WaitGroup
		wg.Add(2)
		barrier := make(chan struct{})
		var addErr, delErr error

		go func() {
			defer wg.Done()
			<-barrier
			_, addErr = s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
				ProfileId: profileID, ExpectedVersion: 1,
				Entry: &cpv1.ProfileEntry{
					Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "x",
					Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF, CatalogId: catID,
				},
			}))
		}()
		go func() {
			defer wg.Done()
			<-barrier
			_, delErr = s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{
				CatalogId: catID,
			}))
		}()
		close(barrier)
		wg.Wait()

		addCode := connect.CodeOf(addErr)
		delCode := connect.CodeOf(delErr)
		if addErr != nil && addCode != connect.CodeNotFound {
			t.Fatalf("iter %d: AddProfileEntry: unexpected error (want nil or NotFound): %v", i, addErr)
		}
		if delErr != nil && delCode != connect.CodeFailedPrecondition {
			t.Fatalf("iter %d: DeleteCatalogEntry: unexpected error (want nil or FailedPrecondition): %v", i, delErr)
		}

		if delErr == nil {
			// Delete won: zero profile entries may reference catID.
			profiles, _, err := s.st.Profiles().CountRefsByCatalogRef(context.Background(), catID)
			if err != nil {
				t.Fatalf("iter %d: CountRefsByCatalogRef: %v", i, err)
			}
			if profiles != 0 {
				t.Fatalf("iter %d: delete succeeded but %d profile(s) still reference %s (dangling ref)", i, profiles, catID)
			}
		} else {
			// Delete was refused: the row must survive, and the attach must have committed —
			// that nonzero ref count is exactly why the delete was refused.
			if _, err := s.st.CustomizationCatalog().Get(context.Background(), catID); err != nil {
				t.Fatalf("iter %d: delete was refused but catalog row is gone: %v", i, err)
			}
			if addErr != nil {
				t.Fatalf("iter %d: delete was refused (implying the attach committed) but AddProfileEntry failed: %v", i, addErr)
			}
		}
	}
}
