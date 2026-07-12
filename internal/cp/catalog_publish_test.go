package cp

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// adminCtx returns an authenticated context for the "admin" owner. Tests using it must call
// s.SetAdminOwners([]string{"admin"}) first — the allowlist fails closed.
func adminCtx() context.Context { return auth.WithOwner(context.Background(), "admin") }

// --- PublishCatalogEntry -------------------------------------------------------

func TestPublishCatalogEntry_NoAdmins_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)
	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	// No admin allowlist configured (fail-closed default) — even the creator cannot publish.
	_, err := s.PublishCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.PublishCatalogEntryRequest{CatalogId: catID}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied with no admins configured, got %v", err)
	}
}

func TestPublishCatalogEntry_Admin_Succeeds(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})
	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	if _, err := s.PublishCatalogEntry(adminCtx(), connect.NewRequest(&cpv1.PublishCatalogEntryRequest{CatalogId: catID})); err != nil {
		t.Fatalf("PublishCatalogEntry: %v", err)
	}

	gr, err := s.GetCatalogEntry(bobCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID}))
	if err != nil {
		t.Fatalf("bob GetCatalogEntry after publish: %v", err)
	}
	if !gr.Msg.Entry.Listed {
		t.Error("expected listed=true after publish")
	}
}

func TestPublishCatalogEntry_NonAdminNonCreator_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})
	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "alice-skill")

	_, err := s.PublishCatalogEntry(bobCtx(), connect.NewRequest(&cpv1.PublishCatalogEntryRequest{CatalogId: catID}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied for a non-admin non-creator, got %v", err)
	}
}

// TestPublishCatalogEntry_AdminGateBeforeExistence verifies the admin gate runs BEFORE any
// existence lookup (S6): a non-admin gets PermissionDenied even for an unknown id, so the error
// code can't be used to probe existence; an admin correctly gets NotFound.
func TestPublishCatalogEntry_AdminGateBeforeExistence(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})

	_, err := s.PublishCatalogEntry(bobCtx(), connect.NewRequest(&cpv1.PublishCatalogEntryRequest{CatalogId: "no-such"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied for non-admin on unknown id, got %v", err)
	}

	_, err = s.PublishCatalogEntry(adminCtx(), connect.NewRequest(&cpv1.PublishCatalogEntryRequest{CatalogId: "no-such"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound for admin on unknown id, got %v", err)
	}
}

// --- UnpublishCatalogEntry -------------------------------------------------------

func TestUnpublishCatalogEntry_Admin_GuardedUnlist(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})
	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")
	if _, err := s.PublishCatalogEntry(adminCtx(), connect.NewRequest(&cpv1.PublishCatalogEntryRequest{CatalogId: catID})); err != nil {
		t.Fatalf("PublishCatalogEntry: %v", err)
	}

	if _, err := s.UnpublishCatalogEntry(adminCtx(), connect.NewRequest(&cpv1.UnpublishCatalogEntryRequest{
		CatalogId: catID, Confirm: true,
	})); err != nil {
		t.Fatalf("UnpublishCatalogEntry: %v", err)
	}

	gr, err := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID}))
	if err != nil {
		t.Fatalf("GetCatalogEntry: %v", err)
	}
	if gr.Msg.Entry.Listed {
		t.Error("expected listed=false after unpublish")
	}
}

func TestUnpublishCatalogEntry_NonAdmin_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)
	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	_, err := s.UnpublishCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.UnpublishCatalogEntryRequest{CatalogId: catID}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", err)
	}
}

// --- PublishBundle -------------------------------------------------------

// seedBundleWithTwoVersions creates a bundle with 2 versions x 2 members each, all unlisted
// (as bundle-member ingest creates them). Returns the bundle id and the 4 member catalog ids.
func seedBundleWithTwoVersions(t *testing.T, s *Server) (bundleID string, catalogIDs []string) {
	t.Helper()
	ctx := context.Background()
	bundleID = uuid.NewString()
	if err := s.st.SkillBundles().Create(ctx, store.SkillBundle{
		BundleID: bundleID, CreatorID: "alice", Name: "bundle", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("SkillBundles().Create: %v", err)
	}
	for seq := int64(1); seq <= 2; seq++ {
		var bundleMembers []store.SkillBundleMember
		for i := 0; i < 2; i++ {
			catID := uuid.NewString()
			subdir := fmt.Sprintf("member-%d-%d", seq, i)
			e := store.CustomizationCatalogEntry{
				CatalogID: catID, CreatorID: "alice", Kind: string(store.ProfileEntrySkill),
				Name: subdir, Listed: false, BundleMember: true,
				CreatedAt: 1, UpdatedAt: 1,
			}
			if err := s.st.CustomizationCatalog().Create(ctx, e); err != nil {
				t.Fatalf("CustomizationCatalog().Create: %v", err)
			}
			bundleMembers = append(bundleMembers, store.SkillBundleMember{
				SourceSubdir: subdir, CatalogID: catID, Position: i,
			})
			catalogIDs = append(catalogIDs, catID)
		}
		versionID := uuid.NewString()
		if err := s.st.SkillBundles().CreateVersion(ctx, store.SkillBundleVersion{
			VersionID: versionID, BundleID: bundleID, Seq: seq, CreatedAt: 1,
		}, bundleMembers); err != nil {
			t.Fatalf("CreateVersion seq=%d: %v", seq, err)
		}
	}
	return bundleID, catalogIDs
}

func TestPublishBundle_Admin_ListsAllVersionMembers(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})
	bundleID, catalogIDs := seedBundleWithTwoVersions(t, s)

	if _, err := s.PublishBundle(adminCtx(), connect.NewRequest(&cpv1.PublishBundleRequest{BundleId: bundleID})); err != nil {
		t.Fatalf("PublishBundle: %v", err)
	}

	if len(catalogIDs) != 4 {
		t.Fatalf("test setup: expected 4 member catalog ids, got %d", len(catalogIDs))
	}
	for _, catID := range catalogIDs {
		gr, err := s.GetCatalogEntry(bobCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID}))
		if err != nil {
			t.Fatalf("bob GetCatalogEntry(%s): %v", catID, err)
		}
		if !gr.Msg.Entry.Listed {
			t.Errorf("catalog entry %s should be listed after PublishBundle (all versions, not just latest)", catID)
		}
	}
}

func TestPublishBundle_NonAdmin_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _ := seedBundleWithTwoVersions(t, s)

	_, err := s.PublishBundle(aliceCtx(), connect.NewRequest(&cpv1.PublishBundleRequest{BundleId: bundleID}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", err)
	}
}

func TestPublishBundle_UnknownID_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})

	_, err := s.PublishBundle(adminCtx(), connect.NewRequest(&cpv1.PublishBundleRequest{BundleId: "no-such"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}
