package cp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/store"
)

// This file holds sp-mwco.1.8's RPC-level tests: AddProfileEntry(BUNDLE_REF) attach/pin/
// overrides/collision/cap, GetProfile's badge enrichment, and RepinProfileBundle's diff-gated
// re-pin. seedBundle/addBundleRefEntry/addBundleRefEntryWithOverrides (profiles_assembly_bundle_test.go)
// and aliceCtx/bobCtx (profiles_test.go) are reused throughout.

// mkCatalogSkill inserts a single bundle-member catalog row (by-ref, unlisted, BundleMember=true).
func mkCatalogSkill(t *testing.T, s *Server, creator, catalogID, name, subdir, sha string) {
	t.Helper()
	if err := s.st.CustomizationCatalog().CreateSkill(context.Background(), store.CustomizationCatalogEntry{
		CatalogID: catalogID, CreatorID: creator, Kind: string(store.ProfileEntrySkill), Name: name,
		Description: "bundle member", Listed: false, CreatedAt: 1000, UpdatedAt: 1000,
		SourceSubdir: subdir, SHA256: &sha, BundleMember: true,
	}); err != nil {
		t.Fatalf("CreateSkill %s: %v", catalogID, err)
	}
}

// mkBundleVersion creates a skill_bundle_version row with the given seq + members.
func mkBundleVersion(t *testing.T, s *Server, bundleID string, seq int64, members []store.SkillBundleMember) string {
	t.Helper()
	versionID := fmt.Sprintf("%s-v%d", bundleID, seq)
	if err := s.st.SkillBundles().CreateVersion(context.Background(), store.SkillBundleVersion{
		VersionID: versionID, BundleID: bundleID, Seq: seq, CreatedAt: 1000,
	}, members); err != nil {
		t.Fatalf("CreateVersion seq %d: %v", seq, err)
	}
	return versionID
}

// mkBundle creates a bare skill_bundle row (no versions).
func mkBundle(t *testing.T, s *Server, bundleID, creator string) {
	t.Helper()
	if err := s.st.SkillBundles().Create(context.Background(), store.SkillBundle{
		BundleID: bundleID, CreatorID: creator, Name: bundleID, SourceURL: "https://example.com/" + bundleID,
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("SkillBundles().Create: %v", err)
	}
}

// mintAndViewDiffToken mints a diff token via the in-memory gate and immediately marks it
// viewed, mirroring a ReingestBundle -> GetBundleDiff round trip without needing a real HTTP
// fetch (sp-mwco.1.7's fetcher stubbing lives in bundles_test.go; this task only needs the gate).
func mintAndViewDiffToken(s *Server, owner, bundleID, fromVersionID, toVersionID string) string {
	s.bundleDiffGate.mint(owner, bundleID, fromVersionID, toVersionID, time.Now())
	return s.bundleDiffGate.markViewed(owner, bundleID, fromVersionID, toVersionID, time.Now())
}

// ---- AddProfileEntry(BUNDLE_REF): attach/pin ---------------------------------------------------

func TestAddProfileEntry_BundleRef_HappyPathPinsLatest(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, versionID, _, _ := seedBundle(t, s, "alice", 3)
	profileID := createProfile(t, s)

	resp, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "my-bundle",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
		},
	}))
	if err != nil {
		t.Fatalf("AddProfileEntry: %v", err)
	}
	if len(resp.Msg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", resp.Msg.Warnings)
	}

	_, entries := loadProfile(t, s, profileID)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.SourceKind != store.ProfileSourceBundle || e.BundleID != bundleID || e.VersionID != versionID {
		t.Errorf("unexpected pin: SourceKind=%q BundleID=%q VersionID=%q, want bundle/%s/%s", e.SourceKind, e.BundleID, e.VersionID, bundleID, versionID)
	}
}

func TestAddProfileEntry_BundleRef_UnknownBundle_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	profileID := createProfile(t, s)

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "x",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: "no-such-bundle",
		},
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestAddProfileEntry_BundleRef_ForeignBundle_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, _ := seedBundle(t, s, "alice", 1) // alice's bundle

	// bob cannot attach alice's bundle to bob's own profile.
	bobProfile, err := s.CreateProfile(bobCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "bob-p"}))
	if err != nil {
		t.Fatalf("CreateProfile(bob): %v", err)
	}
	_, err = s.AddProfileEntry(bobCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: bobProfile.Msg.ProfileId, ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "x",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
		},
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound for a foreign bundle, got %v", err)
	}
}

func TestAddProfileEntry_BundleRef_NonSkillKind_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, _ := seedBundle(t, s, "alice", 1)
	profileID := createProfile(t, s)

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP, Name: "x",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for non-SKILL kind, got %v", err)
	}
}

func TestAddProfileEntry_BundleRef_CatalogIdSet_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, _ := seedBundle(t, s, "alice", 1)
	profileID := createProfile(t, s)

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "x",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
			CatalogId: "should-be-empty",
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument when catalog_id is set, got %v", err)
	}
}

func TestAddProfileEntry_BundleRef_NonLatestVersion_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, v1, _, _ := seedBundle(t, s, "alice", 1)
	mkCatalogSkill(t, s, "alice", "bnd-nlv-cat2", "member-2", "skills/member-2", fmt.Sprintf("%064x", 99))
	mkBundleVersion(t, s, bundleID, 2, []store.SkillBundleMember{{SourceSubdir: "skills/member-2", CatalogID: "bnd-nlv-cat2", Position: 0}})
	profileID := createProfile(t, s)

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "x",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
			VersionId: v1, // not the latest (v2 is)
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument pinning a non-latest version, got %v", err)
	}
}

func TestAddProfileEntry_BundleRef_UnknownExcludeSubdir_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, _ := seedBundle(t, s, "alice", 2)
	profileID := createProfile(t, s)

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "x",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
			ExcludedSubdirs: []string{"skills/no-such-member"},
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for unknown exclude subdir, got %v", err)
	}
}

func TestAddProfileEntry_BundleRef_UnknownRenameSubdir_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, _ := seedBundle(t, s, "alice", 2)
	profileID := createProfile(t, s)

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "x",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
			MemberRenames: map[string]string{"skills/no-such-member": "renamed"},
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for unknown rename subdir, got %v", err)
	}
}

func TestAddProfileEntry_BundleRef_BadRenameValue_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, names := seedBundle(t, s, "alice", 1)
	subdir := fmt.Sprintf("skills/%s", names[0])

	for _, bad := range []string{"../escape", "a/b", "", "."} {
		t.Run(bad, func(t *testing.T) {
			profileID := createProfile(t, s)
			_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
				ProfileId: profileID, ExpectedVersion: 1,
				Entry: &cpv1.ProfileEntry{
					Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "x",
					Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
					MemberRenames: map[string]string{subdir: bad},
				},
			}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("rename value %q: expected InvalidArgument, got %v", bad, err)
			}
		})
	}
}

func TestAddProfileEntry_BundleRef_AllExcluded_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, names := seedBundle(t, s, "alice", 2)
	profileID := createProfile(t, s)

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "x",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
			ExcludedSubdirs: []string{fmt.Sprintf("skills/%s", names[0]), fmt.Sprintf("skills/%s", names[1])},
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument when every member is excluded, got %v", err)
	}
}

func TestAddProfileEntry_BundleRef_ExplicitRenameCollision_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, names := seedBundle(t, s, "alice", 1)
	subdir := fmt.Sprintf("skills/%s", names[0])
	profileID := createProfile(t, s)

	// A standalone custom entry already claims "taken".
	ver := addEntryReturningVer(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "taken",
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
	})

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "my-bundle",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
			MemberRenames: map[string]string{subdir: "taken"}, // explicit + colliding -> InvalidArgument
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for an explicit colliding rename, got %v", err)
	}
}

// TestAddProfileEntry_BundleRef_ImplicitCollision_WarnAndRename is the sp-mwco.1.8 acceptance
// criterion's attach-time half: a bundle member's natural name collides with an existing
// standalone entry -> attach SUCCEEDS with a warning and a persisted rename override, and the
// resulting profile assembles cleanly end-to-end.
func TestAddProfileEntry_BundleRef_ImplicitCollision_WarnAndRename(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, names := seedBundle(t, s, "alice", 1)
	memberName := names[0]
	subdir := fmt.Sprintf("skills/%s", memberName)
	profileID := createProfile(t, s)

	ver := addEntryReturningVer(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: memberName,
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
	})

	resp, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "my-bundle",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
		},
	}))
	if err != nil {
		t.Fatalf("AddProfileEntry: %v (implicit collision must warn-and-rename, not fail)", err)
	}
	if len(resp.Msg.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %v", resp.Msg.Warnings)
	}
	wantName := memberName + "-2"

	p, entries := loadProfile(t, s, profileID)
	var bundleEntry store.ProfileEntry
	for _, e := range entries {
		if e.SourceKind == store.ProfileSourceBundle {
			bundleEntry = e
		}
	}
	if bundleEntry.RenameSubdirs[subdir] != wantName {
		t.Fatalf("RenameSubdirs[%q] = %q, want %q", subdir, bundleEntry.RenameSubdirs[subdir], wantName)
	}

	// The resulting profile must assemble cleanly end-to-end.
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts after implicit-collision attach: %v", err)
	}
	if len(got) != 3 { // manifest + standalone entry + renamed bundle member
		t.Fatalf("want 3 specs, got %d", len(got))
	}
}

// addEntryReturningVer is a thin wrapper over addEntry returning just the new version.
func addEntryReturningVer(t *testing.T, s *Server, profileID string, ver uint64, e *cpv1.ProfileEntry) uint64 {
	t.Helper()
	_, newVer := addEntry(t, s, profileID, ver, e)
	return newVer
}

// ---- AddProfileEntry(BUNDLE_REF): attach-time cap ----------------------------------------------

func TestAddProfileEntry_BundleRef_Cap_ExceedsExpandedBudget_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	// 30 standalone entries already in the profile, then a 40-member bundle -> 70 > 63.
	profileID := createProfile(t, s)
	ver := uint64(1)
	for i := 0; i < 30; i++ {
		ver = addEntryReturningVer(t, s, profileID, ver, &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: fmt.Sprintf("standalone-%d", i),
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
		})
	}
	bundleID, _, _, _ := seedBundle(t, s, "alice", 40)

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "my-bundle",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument over the expanded artifact cap, got %v", err)
	}
}

func TestAddProfileEntry_BundleRef_Cap_WithinBudget_Succeeds(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, _ := seedBundle(t, s, "alice", 63)
	profileID := createProfile(t, s)

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "my-bundle",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
		},
	}))
	if err != nil {
		t.Fatalf("AddProfileEntry: %v (63 members should fit the 63-artifact budget exactly)", err)
	}
}

// ---- GetProfile: bundle_ref badge enrichment ----------------------------------------------------

func TestGetProfile_BundleRef_Badge(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, v1, _, names := seedBundle(t, s, "alice", 3)
	profileID := createProfile(t, s)
	addBundleRefEntryWithOverrides(t, s, profileID, 1, "ent-bundle", "my-bundle", bundleID, v1, nil,
		[]string{fmt.Sprintf("skills/%s", names[0])}, nil)

	// Cut a second version (seq 2) — re-ingestion must NOT touch the existing profile's pin.
	mkCatalogSkill(t, s, "alice", "badge-cat-new", "member-new", "skills/member-new", fmt.Sprintf("%064x", 77))
	mkBundleVersion(t, s, bundleID, 2, []store.SkillBundleMember{{SourceSubdir: "skills/member-new", CatalogID: "badge-cat-new", Position: 0}})

	resp, err := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if len(resp.Msg.Profile.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp.Msg.Profile.Entries))
	}
	e := resp.Msg.Profile.Entries[0]
	if e.VersionId != v1 {
		t.Fatalf("VersionId = %q, want %q (re-ingestion must not move an existing profile's pin)", e.VersionId, v1)
	}
	if e.PinnedSeq != 1 {
		t.Errorf("PinnedSeq = %d, want 1", e.PinnedSeq)
	}
	if e.LatestSeq != 2 {
		t.Errorf("LatestSeq = %d, want 2", e.LatestSeq)
	}
	if e.MemberCount != 2 { // 3 members, 1 excluded
		t.Errorf("MemberCount = %d, want 2", e.MemberCount)
	}
}

// ---- RepinProfileBundle -------------------------------------------------------------------------

// repinFixture builds a bundle at seq 1 (members a, b) and seq 2 (members a, c — b removed, c
// added), a profile pinned to seq 1 with an exclude override on b and a rename override on a,
// and returns everything the re-pin tests need.
type repinFixture struct {
	bundleID           string
	v1, v2             string
	profileID, entryID string
}

func newRepinFixture(t *testing.T, s *Server) repinFixture {
	t.Helper()
	bundleID := "bnd-" + sanitizeTestName(t.Name())
	mkBundle(t, s, bundleID, "alice")
	mkCatalogSkill(t, s, "alice", bundleID+"-cat-a", "member-a", "skills/a", fmt.Sprintf("%064x", 1))
	mkCatalogSkill(t, s, "alice", bundleID+"-cat-b", "member-b", "skills/b", fmt.Sprintf("%064x", 2))
	v1 := mkBundleVersion(t, s, bundleID, 1, []store.SkillBundleMember{
		{SourceSubdir: "skills/a", CatalogID: bundleID + "-cat-a", Position: 0},
		{SourceSubdir: "skills/b", CatalogID: bundleID + "-cat-b", Position: 1},
	})
	mkCatalogSkill(t, s, "alice", bundleID+"-cat-c", "member-c", "skills/c", fmt.Sprintf("%064x", 3))
	v2 := mkBundleVersion(t, s, bundleID, 2, []store.SkillBundleMember{
		{SourceSubdir: "skills/a", CatalogID: bundleID + "-cat-a", Position: 0},
		{SourceSubdir: "skills/c", CatalogID: bundleID + "-cat-c", Position: 1},
	})

	profileID := createProfile(t, s)
	addBundleRefEntryWithOverrides(t, s, profileID, 1, "ent-bundle", "my-bundle", bundleID, v1, nil,
		nil, map[string]string{"skills/a": "member-a-renamed"})

	return repinFixture{bundleID: bundleID, v1: v1, v2: v2, profileID: profileID, entryID: "ent-bundle"}
}

func TestRepinProfileBundle_NoDiffToken_FailedPrecondition(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := newRepinFixture(t, s)

	_, err := s.RepinProfileBundle(aliceCtx(), connect.NewRequest(&cpv1.RepinProfileBundleRequest{
		ProfileId: f.profileID, EntryId: f.entryID, VersionId: f.v2, ExpectedVersion: 2,
		DiffToken: "",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("expected FailedPrecondition with no diff token, got %v", err)
	}
}

func TestRepinProfileBundle_TokenMintedNotViewed_FailedPrecondition(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := newRepinFixture(t, s)
	tok := s.bundleDiffGate.mint("alice", f.bundleID, f.v1, f.v2, time.Now())

	_, err := s.RepinProfileBundle(aliceCtx(), connect.NewRequest(&cpv1.RepinProfileBundleRequest{
		ProfileId: f.profileID, EntryId: f.entryID, VersionId: f.v2, ExpectedVersion: 2,
		DiffToken: tok,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("expected FailedPrecondition for an unviewed token, got %v", err)
	}
}

func TestRepinProfileBundle_ViewedToken_Succeeds(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := newRepinFixture(t, s)
	tok := mintAndViewDiffToken(s, "alice", f.bundleID, f.v1, f.v2)

	resp, err := s.RepinProfileBundle(aliceCtx(), connect.NewRequest(&cpv1.RepinProfileBundleRequest{
		ProfileId: f.profileID, EntryId: f.entryID, VersionId: f.v2, ExpectedVersion: 2,
		DiffToken: tok,
	}))
	if err != nil {
		t.Fatalf("RepinProfileBundle: %v", err)
	}
	if resp.Msg.Version != 3 {
		t.Errorf("Version = %d, want 3", resp.Msg.Version)
	}

	_, entries := loadProfile(t, s, f.profileID)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.VersionID != f.v2 {
		t.Errorf("VersionID = %q, want %q", e.VersionID, f.v2)
	}
	// "skills/a" survives into v2, so its rename override must be carried forward untouched.
	if e.RenameSubdirs["skills/a"] != "member-a-renamed" {
		t.Errorf("RenameSubdirs[skills/a] = %q, want member-a-renamed (surviving member's override kept)", e.RenameSubdirs["skills/a"])
	}
}

func TestRepinProfileBundle_DropsOverrideForRemovedMember(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := newRepinFixture(t, s)
	// Add an exclude on "skills/b", which v2 removes entirely.
	_, err := s.st.Profiles().UpdateEntryPin(context.Background(), f.profileID, 2, f.entryID, f.v1,
		mustEncodeOverrides(t, nil, map[string]string{"skills/a": "member-a-renamed", "skills/b": "member-b-renamed"}), 2000)
	if err != nil {
		t.Fatalf("seed UpdateEntryPin: %v", err)
	}

	tok := mintAndViewDiffToken(s, "alice", f.bundleID, f.v1, f.v2)
	resp, err := s.RepinProfileBundle(aliceCtx(), connect.NewRequest(&cpv1.RepinProfileBundleRequest{
		ProfileId: f.profileID, EntryId: f.entryID, VersionId: f.v2, ExpectedVersion: 3,
		DiffToken: tok,
	}))
	if err != nil {
		t.Fatalf("RepinProfileBundle: %v", err)
	}
	foundDropWarning := false
	for _, w := range resp.Msg.Warnings {
		if w == `override for removed member "skills/b" dropped` {
			foundDropWarning = true
		}
	}
	if !foundDropWarning {
		t.Errorf("expected a drop warning for skills/b, got %v", resp.Msg.Warnings)
	}

	_, entries := loadProfile(t, s, f.profileID)
	e := entries[0]
	if _, ok := e.RenameSubdirs["skills/b"]; ok {
		t.Errorf("RenameSubdirs still has skills/b, want it dropped (member removed in v2)")
	}
	if e.RenameSubdirs["skills/a"] != "member-a-renamed" {
		t.Errorf("RenameSubdirs[skills/a] = %q, want member-a-renamed (surviving override kept)", e.RenameSubdirs["skills/a"])
	}
}

func TestRepinProfileBundle_NewMemberCollisionAutoRenamed(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := newRepinFixture(t, s)

	// A standalone entry already claims "member-c" — the name v2's new member ("skills/c") would
	// naturally take.
	ver := addEntryReturningVer(t, s, f.profileID, 2, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "member-c",
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
	})

	tok := mintAndViewDiffToken(s, "alice", f.bundleID, f.v1, f.v2)
	resp, err := s.RepinProfileBundle(aliceCtx(), connect.NewRequest(&cpv1.RepinProfileBundleRequest{
		ProfileId: f.profileID, EntryId: f.entryID, VersionId: f.v2, ExpectedVersion: ver,
		DiffToken: tok,
	}))
	if err != nil {
		t.Fatalf("RepinProfileBundle: %v (a new-member collision must auto-rename, not fail)", err)
	}
	found := false
	for _, w := range resp.Msg.Warnings {
		if w == `bundle member "member-c" renamed to "member-c-2" (name already used by another profile entry)` {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an auto-rename warning for member-c, got %v", resp.Msg.Warnings)
	}

	_, entries := loadProfile(t, s, f.profileID)
	var bundleEntry store.ProfileEntry
	for _, e := range entries {
		if e.SourceKind == store.ProfileSourceBundle {
			bundleEntry = e
		}
	}
	if bundleEntry.RenameSubdirs["skills/c"] != "member-c-2" {
		t.Errorf("RenameSubdirs[skills/c] = %q, want member-c-2", bundleEntry.RenameSubdirs["skills/c"])
	}
}

func TestRepinProfileBundle_StaleExpectedVersion_Aborted(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := newRepinFixture(t, s)
	tok := mintAndViewDiffToken(s, "alice", f.bundleID, f.v1, f.v2)

	_, err := s.RepinProfileBundle(aliceCtx(), connect.NewRequest(&cpv1.RepinProfileBundleRequest{
		ProfileId: f.profileID, EntryId: f.entryID, VersionId: f.v2, ExpectedVersion: 1, // stale (actual is 2)
		DiffToken: tok,
	}))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("expected Aborted for a stale expected_version, got %v", err)
	}
}

func TestRepinProfileBundle_BackwardsSeq_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := newRepinFixture(t, s)
	// Re-pin the entry onto v2 first so the pinned seq becomes 2.
	tok := mintAndViewDiffToken(s, "alice", f.bundleID, f.v1, f.v2)
	if _, err := s.RepinProfileBundle(aliceCtx(), connect.NewRequest(&cpv1.RepinProfileBundleRequest{
		ProfileId: f.profileID, EntryId: f.entryID, VersionId: f.v2, ExpectedVersion: 2, DiffToken: tok,
	})); err != nil {
		t.Fatalf("setup RepinProfileBundle to v2: %v", err)
	}

	// Now attempt to go "back" to v1 (seq 1 <= pinned seq 2).
	backTok := mintAndViewDiffToken(s, "alice", f.bundleID, f.v2, f.v1)
	_, err := s.RepinProfileBundle(aliceCtx(), connect.NewRequest(&cpv1.RepinProfileBundleRequest{
		ProfileId: f.profileID, EntryId: f.entryID, VersionId: f.v1, ExpectedVersion: 3,
		DiffToken: backTok,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for a backwards seq re-pin, got %v", err)
	}
}

func TestRepinProfileBundle_EqualSeq_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := newRepinFixture(t, s)
	tok := mintAndViewDiffToken(s, "alice", f.bundleID, f.v1, f.v1)

	_, err := s.RepinProfileBundle(aliceCtx(), connect.NewRequest(&cpv1.RepinProfileBundleRequest{
		ProfileId: f.profileID, EntryId: f.entryID, VersionId: f.v1, ExpectedVersion: 2,
		DiffToken: tok,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for an equal-seq re-pin, got %v", err)
	}
}

// mustEncodeOverrides is a test-only wrapper over store.EncodeBundleOverrides.
func mustEncodeOverrides(t *testing.T, exclude []string, rename map[string]string) string {
	t.Helper()
	s, err := store.EncodeBundleOverrides(exclude, rename)
	if err != nil {
		t.Fatalf("EncodeBundleOverrides: %v", err)
	}
	return s
}

// ---- Re-ingestion never touches an existing profile (acceptance criterion) ----------------------

func TestReingestNeverTouchesExistingProfile(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, v1, _, _ := seedBundle(t, s, "alice", 2)
	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", bundleID, v1, nil)

	mkCatalogSkill(t, s, "alice", "reingest-cat-new", "member-new", "skills/member-new", fmt.Sprintf("%064x", 55))
	mkBundleVersion(t, s, bundleID, 2, []store.SkillBundleMember{{SourceSubdir: "skills/member-new", CatalogID: "reingest-cat-new", Position: 0}})

	_, entries := loadProfile(t, s, profileID)
	if len(entries) != 1 || entries[0].VersionID != v1 {
		t.Fatalf("profile entry moved off its pin after a new version was cut: %+v", entries)
	}
}
