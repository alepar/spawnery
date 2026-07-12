package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"spawnery/internal/cp/store"
)

func makeBundle(bundleID, creatorID, url, ref, subdir string) store.SkillBundle {
	return store.SkillBundle{
		BundleID:     bundleID,
		CreatorID:    creatorID,
		Name:         "a test bundle",
		SourceURL:    url,
		SourceRef:    ref,
		SourceSubdir: subdir,
		CreatedAt:    1000,
		UpdatedAt:    1000,
	}
}

func TestSkillBundle_CreateGetByKey_EmptyRefSubdir(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	b := makeBundle("bundle-1", "alice", "owner/repo", "", "")
	if err := st.SkillBundles().Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := st.SkillBundles().GetByKey(ctx, "alice", "owner/repo", "", "")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	if got.BundleID != "bundle-1" {
		t.Fatalf("BundleID = %q, want bundle-1", got.BundleID)
	}
}

// TestSkillBundle_RepasteIsIdempotencyConflict is the bead's re-paste idempotency proof: creating
// a second bundle with the SAME (creator_id, source_url, source_ref, source_subdir) key — even
// with empty ref/subdir — must conflict, not silently mint a second bundle (which is exactly what
// a NULL-bearing key would have allowed).
func TestSkillBundle_RepasteIsIdempotencyConflict(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	b1 := makeBundle("bundle-1", "alice", "owner/repo", "", "")
	if err := st.SkillBundles().Create(ctx, b1); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	b2 := makeBundle("bundle-2", "alice", "owner/repo", "", "")
	err := st.SkillBundles().Create(ctx, b2)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on re-paste, got: %v", err)
	}
}

func TestSkillBundle_GetByKey_NotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	_, err := st.SkillBundles().GetByKey(ctx, "alice", "owner/repo", "", "")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestSkillBundle_Get_NotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	_, err := st.SkillBundles().Get(ctx, "no-such")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestSkillBundle_ListByCreator(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"b1", "b2"} {
		if err := st.SkillBundles().Create(ctx, makeBundle(id, "alice", "owner/repo-"+id, "", "")); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	if err := st.SkillBundles().Create(ctx, makeBundle("b3", "bob", "owner/repo3", "", "")); err != nil {
		t.Fatalf("Create b3: %v", err)
	}

	aliceBundles, err := st.SkillBundles().ListByCreator(ctx, "alice")
	if err != nil {
		t.Fatalf("ListByCreator: %v", err)
	}
	if len(aliceBundles) != 2 {
		t.Fatalf("expected 2 alice bundles, got %d", len(aliceBundles))
	}
}

func seedCatalogRow(t *testing.T, st store.Store, catalogID, creatorID string, bundleMember bool) {
	t.Helper()
	e := store.CustomizationCatalogEntry{
		CatalogID:    catalogID,
		CreatorID:    creatorID,
		Kind:         string(store.ProfileEntrySkill),
		Name:         catalogID,
		Description:  "a bundle member",
		Listed:       false,
		CreatedAt:    1000,
		UpdatedAt:    1000,
		BundleMember: bundleMember,
	}
	if err := st.CustomizationCatalog().Create(context.Background(), e); err != nil {
		t.Fatalf("seed catalog row %s: %v", catalogID, err)
	}
}

func TestSkillBundle_CreateVersionWithMembers(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.SkillBundles().Create(ctx, makeBundle("bundle-1", "alice", "owner/repo", "", "")); err != nil {
		t.Fatalf("Create bundle: %v", err)
	}
	seedCatalogRow(t, st, "cat-a", "alice", true)
	seedCatalogRow(t, st, "cat-b", "alice", true)

	v := store.SkillBundleVersion{VersionID: "v1", BundleID: "bundle-1", Seq: 1, SourceCommit: "deadbeef", CreatedAt: 1000}
	members := []store.SkillBundleMember{
		{SourceSubdir: "skills/b", CatalogID: "cat-b", Position: 1},
		{SourceSubdir: "skills/a", CatalogID: "cat-a", Position: 0},
	}
	if err := st.SkillBundles().CreateVersion(ctx, v, members); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	got, err := st.SkillBundles().Members(ctx, "v1")
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 members, got %d", len(got))
	}
	if got[0].SourceSubdir != "skills/a" || got[1].SourceSubdir != "skills/b" {
		t.Fatalf("Members not ordered by position ASC: %+v", got)
	}
}

// TestSkillBundle_DuplicateSourceSubdirInVersionConflicts locks in the rename/dup-dir detection:
// a version cannot have two members claiming the same source_subdir.
func TestSkillBundle_DuplicateSourceSubdirInVersionConflicts(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.SkillBundles().Create(ctx, makeBundle("bundle-1", "alice", "owner/repo", "", "")); err != nil {
		t.Fatalf("Create bundle: %v", err)
	}
	seedCatalogRow(t, st, "cat-a", "alice", true)
	seedCatalogRow(t, st, "cat-b", "alice", true)

	v := store.SkillBundleVersion{VersionID: "v1", BundleID: "bundle-1", Seq: 1, CreatedAt: 1000}
	members := []store.SkillBundleMember{
		{SourceSubdir: "skills/x", CatalogID: "cat-a", Position: 0},
		{SourceSubdir: "skills/x", CatalogID: "cat-b", Position: 1},
	}
	err := st.SkillBundles().CreateVersion(ctx, v, members)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for duplicate source_subdir, got: %v", err)
	}
}

// TestSkillBundle_SameCatalogIDDifferentSubdirAllowed: the same catalog_id may appear at two
// different source_subdirs within one version (keying is on source_subdir, not catalog_id).
func TestSkillBundle_SameCatalogIDDifferentSubdirAllowed(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.SkillBundles().Create(ctx, makeBundle("bundle-1", "alice", "owner/repo", "", "")); err != nil {
		t.Fatalf("Create bundle: %v", err)
	}
	seedCatalogRow(t, st, "cat-a", "alice", true)

	v := store.SkillBundleVersion{VersionID: "v1", BundleID: "bundle-1", Seq: 1, CreatedAt: 1000}
	members := []store.SkillBundleMember{
		{SourceSubdir: "skills/x", CatalogID: "cat-a", Position: 0},
		{SourceSubdir: "skills/y", CatalogID: "cat-a", Position: 1},
	}
	if err := st.SkillBundles().CreateVersion(ctx, v, members); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	got, err := st.SkillBundles().Members(ctx, "v1")
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 members, got %d", len(got))
	}
}

func TestSkillBundle_LatestVersion(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.SkillBundles().Create(ctx, makeBundle("bundle-1", "alice", "owner/repo", "", "")); err != nil {
		t.Fatalf("Create bundle: %v", err)
	}
	for _, seq := range []int64{1, 2, 3} {
		v := store.SkillBundleVersion{VersionID: fmt.Sprintf("v%d", seq), BundleID: "bundle-1", Seq: seq, CreatedAt: 1000}
		if err := st.SkillBundles().CreateVersion(ctx, v, nil); err != nil {
			t.Fatalf("CreateVersion seq=%d: %v", seq, err)
		}
	}

	got, err := st.SkillBundles().LatestVersion(ctx, "bundle-1")
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if got.Seq != 3 {
		t.Fatalf("LatestVersion.Seq = %d, want 3", got.Seq)
	}
}

func TestSkillBundle_LatestVersion_NotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.SkillBundles().Create(ctx, makeBundle("bundle-1", "alice", "owner/repo", "", "")); err != nil {
		t.Fatalf("Create bundle: %v", err)
	}

	_, err := st.SkillBundles().LatestVersion(ctx, "bundle-1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestSkillBundle_ListVersions_OrderedBySeq(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.SkillBundles().Create(ctx, makeBundle("bundle-1", "alice", "owner/repo", "", "")); err != nil {
		t.Fatalf("Create bundle: %v", err)
	}
	for _, seq := range []int64{3, 1, 2} {
		v := store.SkillBundleVersion{VersionID: fmt.Sprintf("v%d", seq), BundleID: "bundle-1", Seq: seq, CreatedAt: 1000}
		if err := st.SkillBundles().CreateVersion(ctx, v, nil); err != nil {
			t.Fatalf("CreateVersion seq=%d: %v", seq, err)
		}
	}

	got, err := st.SkillBundles().ListVersions(ctx, "bundle-1")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(got))
	}
	for i, want := range []int64{1, 2, 3} {
		if got[i].Seq != want {
			t.Fatalf("ListVersions[%d].Seq = %d, want %d", i, got[i].Seq, want)
		}
	}
}

func TestSkillBundle_MemberVersionIDs(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.SkillBundles().Create(ctx, makeBundle("bundle-1", "alice", "owner/repo", "", "")); err != nil {
		t.Fatalf("Create bundle: %v", err)
	}
	seedCatalogRow(t, st, "cat-shared", "alice", true)
	seedCatalogRow(t, st, "cat-other", "alice", true)

	v1 := store.SkillBundleVersion{VersionID: "v1", BundleID: "bundle-1", Seq: 1, CreatedAt: 1000}
	if err := st.SkillBundles().CreateVersion(ctx, v1, []store.SkillBundleMember{
		{SourceSubdir: "skills/shared", CatalogID: "cat-shared", Position: 0},
	}); err != nil {
		t.Fatalf("CreateVersion v1: %v", err)
	}
	v2 := store.SkillBundleVersion{VersionID: "v2", BundleID: "bundle-1", Seq: 2, CreatedAt: 1000}
	if err := st.SkillBundles().CreateVersion(ctx, v2, []store.SkillBundleMember{
		{SourceSubdir: "skills/shared", CatalogID: "cat-shared", Position: 0},
		{SourceSubdir: "skills/other", CatalogID: "cat-other", Position: 1},
	}); err != nil {
		t.Fatalf("CreateVersion v2: %v", err)
	}

	got, err := st.SkillBundles().MemberVersionIDs(ctx, "cat-shared")
	if err != nil {
		t.Fatalf("MemberVersionIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 version_ids referencing cat-shared, got %d: %v", len(got), got)
	}

	gotOther, err := st.SkillBundles().MemberVersionIDs(ctx, "cat-other")
	if err != nil {
		t.Fatalf("MemberVersionIDs cat-other: %v", err)
	}
	if len(gotOther) != 1 || gotOther[0] != "v2" {
		t.Fatalf("expected [v2] for cat-other, got %v", gotOther)
	}
}

// TestSkillBundle_VersionDeleteCascadesMembers proves the FK ON DELETE CASCADE from
// skill_bundle_version -> skill_bundle_member under foreign_keys(1): deleting the owning bundle
// cascades through versions to members.
func TestSkillBundle_VersionDeleteCascadesMembers(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.SkillBundles().Create(ctx, makeBundle("bundle-1", "alice", "owner/repo", "", "")); err != nil {
		t.Fatalf("Create bundle: %v", err)
	}
	seedCatalogRow(t, st, "cat-a", "alice", true)
	v := store.SkillBundleVersion{VersionID: "v1", BundleID: "bundle-1", Seq: 1, CreatedAt: 1000}
	if err := st.SkillBundles().CreateVersion(ctx, v, []store.SkillBundleMember{
		{SourceSubdir: "skills/a", CatalogID: "cat-a", Position: 0},
	}); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	if err := store.DeleteBundleForTest(ctx, st, "bundle-1"); err != nil {
		t.Fatalf("DeleteBundleForTest: %v", err)
	}

	versions, err := st.SkillBundles().ListVersions(ctx, "bundle-1")
	if err != nil {
		t.Fatalf("ListVersions after delete: %v", err)
	}
	if len(versions) != 0 {
		t.Fatalf("versions remaining after bundle delete = %d, want 0", len(versions))
	}
	members, err := st.SkillBundles().Members(ctx, "v1")
	if err != nil {
		t.Fatalf("Members after delete: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("members remaining after bundle delete = %d, want 0", len(members))
	}
}

// TestSkillBundle_ContentHitNeverMutatesListedOrName locks in the §4.3 rule at the store layer: a
// content-identity hit (CreateSkill conflict on (creator_id, sha256)) never mutates Listed, Name,
// SourceSubdir, or BundleMember on the existing row — reusing a standalone skill as a bundle
// member must never flip Listed (that would fire the kill-switch on the user's running spawns).
func TestSkillBundle_ContentHitNeverMutatesListedOrName(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	sha := "shared-content-sha"
	original := store.CustomizationCatalogEntry{
		CatalogID:    "cat-orig",
		CreatorID:    "alice",
		Kind:         string(store.ProfileEntrySkill),
		Name:         "original-name",
		Description:  "first ingest",
		Listed:       false,
		CreatedAt:    1000,
		UpdatedAt:    1000,
		SourceSubdir: "skills/orig",
		SHA256:       &sha,
		BundleMember: false,
	}
	if err := st.CustomizationCatalog().CreateSkill(ctx, original); err != nil {
		t.Fatalf("first CreateSkill: %v", err)
	}

	conflict := original
	conflict.CatalogID = "cat-conflict"
	conflict.Name = "renamed-on-reuse"
	conflict.SourceSubdir = "skills/reused"
	conflict.BundleMember = true
	err := st.CustomizationCatalog().CreateSkill(ctx, conflict)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on (creator_id, sha256) hit, got: %v", err)
	}

	got, err := st.CustomizationCatalog().GetByCreatorSHA(ctx, "alice", sha)
	if err != nil {
		t.Fatalf("GetByCreatorSHA: %v", err)
	}
	if got.CatalogID != "cat-orig" {
		t.Fatalf("CatalogID = %q, want cat-orig (existing row untouched)", got.CatalogID)
	}
	if got.Listed {
		t.Fatal("Listed mutated by a content-identity hit")
	}
	if got.Name != "original-name" {
		t.Fatalf("Name mutated: got %q, want original-name", got.Name)
	}
	if got.SourceSubdir != "skills/orig" {
		t.Fatalf("SourceSubdir mutated: got %q, want skills/orig", got.SourceSubdir)
	}
	if got.BundleMember {
		t.Fatal("BundleMember mutated by a content-identity hit")
	}
}
