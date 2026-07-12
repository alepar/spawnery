package cp

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/agentinstall/spec"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

// ---- helpers (sp-mwco.1.5: bundle_ref assembly) -----------------------------

// seedBundle inserts n by-ref catalog rows (SHA256 set, Content nil, Listed=false,
// BundleMember=true) tagged with source_subdir skills/<name>, then a single skill_bundle +
// skill_bundle_version (seq 1) whose members are those n rows in position order. Returns the
// bundle/version ids plus the per-member catalog ids and display names (index-aligned, position
// order).
func seedBundle(t *testing.T, s *Server, creator string, n int) (bundleID, versionID string, catalogIDs, names []string) {
	t.Helper()
	ctx := context.Background()
	tag := sanitizeTestName(t.Name())

	bundleID = "bnd-" + tag
	versionID = "bndv-" + tag
	if err := s.st.SkillBundles().Create(ctx, store.SkillBundle{
		BundleID:  bundleID,
		CreatorID: creator,
		Name:      "test bundle " + tag,
		SourceURL: "https://github.com/example/" + tag,
		CreatedAt: 1000,
		UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("SkillBundles().Create: %v", err)
	}

	members := make([]store.SkillBundleMember, 0, n)
	catalogIDs = make([]string, 0, n)
	names = make([]string, 0, n)
	for i := 0; i < n; i++ {
		catalogID := fmt.Sprintf("%s-cat-%d", tag, i)
		name := fmt.Sprintf("%s-skill-%d", tag, i)
		subdir := fmt.Sprintf("skills/%s", name)
		sha := fmt.Sprintf("%064x", i+1)
		if err := s.st.CustomizationCatalog().CreateSkill(ctx, store.CustomizationCatalogEntry{
			CatalogID:    catalogID,
			CreatorID:    creator,
			Kind:         string(store.ProfileEntrySkill),
			Name:         name,
			Description:  "bundle member",
			Listed:       false,
			CreatedAt:    1000,
			UpdatedAt:    1000,
			SourceSubdir: subdir,
			SHA256:       &sha,
			BundleMember: true,
		}); err != nil {
			t.Fatalf("CreateSkill %s: %v", catalogID, err)
		}
		catalogIDs = append(catalogIDs, catalogID)
		names = append(names, name)
		members = append(members, store.SkillBundleMember{
			SourceSubdir: subdir,
			CatalogID:    catalogID,
			Position:     i,
		})
	}

	if err := s.st.SkillBundles().CreateVersion(ctx, store.SkillBundleVersion{
		VersionID: versionID,
		BundleID:  bundleID,
		Seq:       1,
		CreatedAt: 1000,
	}, members); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	return bundleID, versionID, catalogIDs, names
}

// sanitizeTestName turns a (sub)test name into a string safe for use as an id/URL component.
func sanitizeTestName(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// addBundleRefEntry adds a bundle_ref ProfileEntry directly via the store (no RPC path exists
// for bundle_ref entries until sp-mwco.1.7/1.8 — this is intentional per the sp-mwco.1.5 plan).
func addBundleRefEntry(t *testing.T, s *Server, profileID string, ver uint64, entryID, name, bundleID, versionID string, targets []string) uint64 {
	t.Helper()
	newVer, err := s.st.Profiles().AddEntry(context.Background(), profileID, ver, store.ProfileEntry{
		ProfileID:  profileID,
		EntryID:    entryID,
		Kind:       store.ProfileEntrySkill,
		Name:       name,
		SourceKind: store.ProfileSourceBundle,
		BundleID:   bundleID,
		VersionID:  versionID,
		Targets:    targets,
	}, 2000)
	if err != nil {
		t.Fatalf("AddEntry (bundle_ref): %v", err)
	}
	return newVer
}

// findArtifactByName returns the manifest artifact with the given Name, or nil.
func findArtifactByName(m spec.Manifest, name string) *spec.Artifact {
	for i := range m.Artifacts {
		if m.Artifacts[i].Name == name {
			return &m.Artifacts[i]
		}
	}
	return nil
}

// ---- tests --------------------------------------------------------------------

// TestAssembleBundleRefEmitsArtifactPerMember is the sp-mwco.1.5 acceptance criterion: a 20-member
// bundle emits 20 disjoint artifacts with distinct Ids/DestPaths, and the manifest's skill Names
// are the members' own catalog Names (not the bundle entry's display Name).
func TestAssembleBundleRefEmitsArtifactPerMember(t *testing.T) {
	s, _, _ := newTestServer(t)
	const n = 20

	bundleID, versionID, catalogIDs, names := seedBundle(t, s, "alice", n)

	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", bundleID, versionID, []string{"claude"})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if len(got) != n+1 {
		t.Fatalf("want %d specs (manifest + %d members), got %d", n+1, n, len(got))
	}

	ms := findManifestSpec(got)
	if ms == nil {
		t.Fatal("no manifest spec")
	}
	m := unmarshalManifest(t, ms.Inline)
	if len(m.Artifacts) != n {
		t.Fatalf("want %d artifacts in manifest, got %d", n, len(m.Artifacts))
	}

	seenIDs := map[string]bool{}
	seenDest := map[string]bool{}
	for i, catalogID := range catalogIDs {
		wantID := "ent-bundle/" + catalogID
		wantDest := "payloads/" + wantID

		var payload *cpv1.ArtifactSpec
		for _, spec := range got {
			if spec.Id == wantID {
				payload = spec
				break
			}
		}
		if payload == nil {
			t.Fatalf("member %d: no payload spec with Id %q", i, wantID)
		}
		if payload.DestPath != wantDest {
			t.Errorf("member %d: DestPath = %q, want %q", i, payload.DestPath, wantDest)
		}
		if payload.ContentType != cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR {
			t.Errorf("member %d: ContentType = %v, want TAR", i, payload.ContentType)
		}
		ref := payload.GetObjectref()
		if ref == nil {
			t.Fatalf("member %d: no Objectref (want by-ref delivery)", i)
		}
		if ref.Sha256 == "" || ref.ObjectKey != skillstore.ObjectKey(ref.Sha256) {
			t.Errorf("member %d: Objectref = %+v, ObjectKey inconsistent with Sha256", i, ref)
		}
		if len(payload.Inline) != 0 {
			t.Errorf("member %d: Inline must be empty for by-ref delivery", i)
		}

		if seenIDs[wantID] {
			t.Errorf("member %d: duplicate Id %q", i, wantID)
		}
		seenIDs[wantID] = true
		if seenDest[wantDest] {
			t.Errorf("member %d: duplicate DestPath %q", i, wantDest)
		}
		seenDest[wantDest] = true

		a := findArtifactByName(m, names[i])
		if a == nil {
			t.Fatalf("member %d: no manifest artifact named %q (member catalog Name, not bundle entry Name)", i, names[i])
		}
		if a.Kind != spec.KindSkill {
			t.Errorf("member %d: Kind = %q, want skill", i, a.Kind)
		}
		if a.Skill == nil || a.Skill.Dir != wantDest {
			t.Errorf("member %d: Skill.Dir = %+v, want %q", i, a.Skill, wantDest)
		}
		if len(a.Targets) != 1 || a.Targets[0] != "claude" {
			t.Errorf("member %d: Targets = %v, want [claude] (propagated from bundle entry)", i, a.Targets)
		}
	}
	if len(seenIDs) != n || len(seenDest) != n {
		t.Fatalf("want %d distinct ids/dests, got %d ids, %d dests", n, len(seenIDs), len(seenDest))
	}
}

// TestAssembleBundleRefMixedWithStandaloneEntries: a bundle (3 members) plus a catalog_ref skill
// and a custom inline skill in the same profile all assemble to disjoint payloads.
func TestAssembleBundleRefMixedWithStandaloneEntries(t *testing.T) {
	s, _, _ := newTestServer(t)

	bundleID, versionID, _, _ := seedBundle(t, s, "alice", 3)

	catResp, err := s.CreateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.CreateCatalogEntryRequest{
		Kind:    cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:    "standalone-catalog-skill",
		Content: validSkillTar(),
	}))
	if err != nil {
		t.Fatalf("CreateCatalogEntry: %v", err)
	}

	profileID := createProfile(t, s)
	ver := addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", bundleID, versionID, nil)
	_, ver = addEntry(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "standalone-catalog-skill",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: catResp.Msg.CatalogId,
	})
	_, _ = addEntry(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         "standalone-custom-skill",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: validSkillTar(),
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if len(got) != 6 { // manifest + 3 bundle members + 2 standalone
		t.Fatalf("want 6 specs, got %d: %v", len(got), got)
	}

	dests := map[string]bool{}
	for _, spec := range got {
		if spec.Id == "manifest" {
			continue
		}
		if dests[spec.DestPath] {
			t.Fatalf("duplicate DestPath %q", spec.DestPath)
		}
		dests[spec.DestPath] = true
	}
	if len(dests) != 5 {
		t.Fatalf("want 5 distinct payload dest paths, got %d", len(dests))
	}
}

// TestAssembleBundleMemberNamesCollideWithinBundle: two members of the same bundle version share
// a catalog Name — the fully-expanded duplicate-dir-name check must reject it, even though the
// members have distinct catalog_ids and distinct source_subdirs (ingest's pre-dedup check would
// not have caught this).
func TestAssembleBundleMemberNamesCollideWithinBundle(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()

	bundleID := "bnd-collide-in"
	versionID := "bndv-collide-in"
	if err := s.st.SkillBundles().Create(ctx, store.SkillBundle{
		BundleID: bundleID, CreatorID: "alice", Name: "collider", SourceURL: "https://example.com/collider",
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("SkillBundles().Create: %v", err)
	}
	sha1, sha2 := fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2)
	if err := s.st.CustomizationCatalog().CreateSkill(ctx, store.CustomizationCatalogEntry{
		CatalogID: "cat-1", CreatorID: "alice", Kind: string(store.ProfileEntrySkill), Name: "same-name",
		Listed: false, CreatedAt: 1000, UpdatedAt: 1000, SourceSubdir: "skills/one", SHA256: &sha1, BundleMember: true,
	}); err != nil {
		t.Fatalf("CreateSkill cat-1: %v", err)
	}
	if err := s.st.CustomizationCatalog().CreateSkill(ctx, store.CustomizationCatalogEntry{
		CatalogID: "cat-2", CreatorID: "alice", Kind: string(store.ProfileEntrySkill), Name: "same-name",
		Listed: false, CreatedAt: 1000, UpdatedAt: 1000, SourceSubdir: "skills/two", SHA256: &sha2, BundleMember: true,
	}); err != nil {
		t.Fatalf("CreateSkill cat-2: %v", err)
	}
	if err := s.st.SkillBundles().CreateVersion(ctx, store.SkillBundleVersion{
		VersionID: versionID, BundleID: bundleID, Seq: 1, CreatedAt: 1000,
	}, []store.SkillBundleMember{
		{SourceSubdir: "skills/one", CatalogID: "cat-1", Position: 0},
		{SourceSubdir: "skills/two", CatalogID: "cat-2", Position: 1},
	}); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", bundleID, versionID, nil)

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for within-bundle name collision, got %v", err)
	}
}

// TestAssembleBundleMemberNameCollidesWithStandaloneEntry: a bundle member's catalog Name equals
// a standalone entry's directory name — must reject with the same duplicate-dir-name error, even
// though the bundle entry's own display Name differs.
func TestAssembleBundleMemberNameCollidesWithStandaloneEntry(t *testing.T) {
	s, _, _ := newTestServer(t)

	bundleID, versionID, _, names := seedBundle(t, s, "alice", 1)

	profileID := createProfile(t, s)
	ver := addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", bundleID, versionID, nil)
	_, _ = addEntry(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         names[0], // collides with the bundle member's on-disk name
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: validSkillTar(),
	})

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for bundle/standalone name collision, got %v", err)
	}
}

// TestAssembleBundleRefEmptyVersionID: a bundle_ref entry with no pinned version → InvalidArgument.
func TestAssembleBundleRefEmptyVersionID(t *testing.T) {
	s, _, _ := newTestServer(t)
	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", "", "", nil)

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for empty version_id, got %v", err)
	}
}

// TestAssembleBundleRefUnknownVersionID: a bundle_ref entry pinned to a version that does not
// exist → InvalidArgument.
func TestAssembleBundleRefUnknownVersionID(t *testing.T) {
	s, _, _ := newTestServer(t)
	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", "bnd-x", "no-such-version", nil)

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for unknown version_id, got %v", err)
	}
}

// TestAssembleBundleRefVersionHasNoMembers: a pinned version with zero members → InvalidArgument.
func TestAssembleBundleRefVersionHasNoMembers(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()

	if err := s.st.SkillBundles().Create(ctx, store.SkillBundle{
		BundleID: "bnd-empty", CreatorID: "alice", Name: "empty", SourceURL: "https://example.com/empty",
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("SkillBundles().Create: %v", err)
	}
	if err := s.st.SkillBundles().CreateVersion(ctx, store.SkillBundleVersion{
		VersionID: "bndv-empty", BundleID: "bnd-empty", Seq: 1, CreatedAt: 1000,
	}, nil); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", "bnd-empty", "bndv-empty", nil)

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for zero-member version, got %v", err)
	}
}

// TestAssembleBundleRefMemberCatalogRowMissing: a member row's catalog_id references a catalog
// row that no longer exists (no FK — see skill_bundles.go) → InvalidArgument, not a panic/500.
func TestAssembleBundleRefMemberCatalogRowMissing(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()

	if err := s.st.SkillBundles().Create(ctx, store.SkillBundle{
		BundleID: "bnd-ghost", CreatorID: "alice", Name: "ghost", SourceURL: "https://example.com/ghost",
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("SkillBundles().Create: %v", err)
	}
	if err := s.st.SkillBundles().CreateVersion(ctx, store.SkillBundleVersion{
		VersionID: "bndv-ghost", BundleID: "bnd-ghost", Seq: 1, CreatedAt: 1000,
	}, []store.SkillBundleMember{
		{SourceSubdir: "skills/ghost", CatalogID: "no-such-catalog-row", Position: 0},
	}); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", "bnd-ghost", "bndv-ghost", nil)

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for missing member catalog row, got %v", err)
	}
}

// TestAssembleBundleRefMemberNotASkill: a member catalog row whose Kind isn't "skill" →
// InvalidArgument.
func TestAssembleBundleRefMemberNotASkill(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()

	if err := s.st.CustomizationCatalog().Create(ctx, store.CustomizationCatalogEntry{
		CatalogID: "cat-mcp-member", CreatorID: "alice", Kind: string(store.ProfileEntryMCP), Name: "not-a-skill",
		Listed: false, CreatedAt: 1000, UpdatedAt: 1000, BundleMember: true,
	}); err != nil {
		t.Fatalf("CustomizationCatalog().Create: %v", err)
	}
	if err := s.st.SkillBundles().Create(ctx, store.SkillBundle{
		BundleID: "bnd-notskill", CreatorID: "alice", Name: "notskill", SourceURL: "https://example.com/notskill",
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("SkillBundles().Create: %v", err)
	}
	if err := s.st.SkillBundles().CreateVersion(ctx, store.SkillBundleVersion{
		VersionID: "bndv-notskill", BundleID: "bnd-notskill", Seq: 1, CreatedAt: 1000,
	}, []store.SkillBundleMember{
		{SourceSubdir: "skills/notskill", CatalogID: "cat-mcp-member", Position: 0},
	}); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", "bnd-notskill", "bndv-notskill", nil)

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for a non-skill bundle member, got %v", err)
	}
}

// TestAssembleBundleRefUnlistedMembersAssemble guards against a "fix" that filters assembly on
// Listed: bundle member rows are created Listed=false by design (§4.3 — revocation, not initial
// listing, is what flips Listed), so assembly must succeed for them regardless.
func TestAssembleBundleRefUnlistedMembersAssemble(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, versionID, catalogIDs, _ := seedBundle(t, s, "alice", 3)

	for _, catalogID := range catalogIDs {
		ce, err := s.st.CustomizationCatalog().Get(context.Background(), catalogID)
		if err != nil {
			t.Fatalf("Get %s: %v", catalogID, err)
		}
		if ce.Listed {
			t.Fatalf("member %s: Listed = true, want false (seedBundle invariant)", catalogID)
		}
	}

	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", bundleID, versionID, nil)

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if len(got) != 4 { // manifest + 3 members
		t.Fatalf("want 4 specs, got %d", len(got))
	}
}

// TestBundleExpansionCountsAgainstArtifactCap proves the authoritative 64-artifact cap
// (validateAndMergeArtifacts, called from CreateSpawn) also bounds bundle expansion: a 64-member
// bundle assembles to 65 specs (manifest + 64), which exceeds the cap.
func TestBundleExpansionCountsAgainstArtifactCap(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, versionID, _, _ := seedBundle(t, s, "alice", 64)

	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bundle", "my-bundle", bundleID, versionID, nil)

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if len(got) != 65 {
		t.Fatalf("want 65 specs (manifest + 64 members), got %d", len(got))
	}

	_, err = validateAndMergeArtifacts(nil, got)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument (too many artifacts), got %v", err)
	}
}
