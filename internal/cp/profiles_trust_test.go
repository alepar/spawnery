package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/store"
)

// This file holds sp-mwco.2.8's tests: the UpdateProfileEntry handler (targets + disabled
// scoping-only mutation), assembly-skips-disabled, provenance-in-manifest, and
// provenance-on-GetProfile. It reuses createProfile/addEntry/loadProfile/aliceCtx/bobCtx/
// createTestCatalogEntry/allTargetNames from profiles_test.go / profiles_assembly_test.go /
// customization_catalog_test.go.

// --- UpdateProfileEntry ------------------------------------------------------

func TestUpdateProfileEntry_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "sk")
	profileID := createProfile(t, s)
	entryID, ver := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "sk",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: catID,
		Targets:   []string{"claude"},
	})

	resp, err := s.UpdateProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: ver,
		EntryId:         entryID,
		Targets:         []string{"claude", "goose"},
		Disabled:        true,
	}))
	if err != nil {
		t.Fatalf("UpdateProfileEntry: %v", err)
	}
	if resp.Msg.Version != ver+1 {
		t.Errorf("Version = %d, want %d", resp.Msg.Version, ver+1)
	}

	_, entries := loadProfile(t, s, profileID)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.EntryID != entryID {
		t.Errorf("EntryID changed: got %q want %q", e.EntryID, entryID)
	}
	if !e.Disabled {
		t.Error("expected Disabled=true")
	}
	if len(e.Targets) != 2 || e.Targets[0] != "claude" || e.Targets[1] != "goose" {
		t.Errorf("Targets = %v, want [claude goose]", e.Targets)
	}
	// Every other column preserved.
	if e.Kind != store.ProfileEntrySkill || e.Name != "sk" ||
		e.SourceKind != store.ProfileSourceCatalog || e.CatalogID != catID {
		t.Errorf("UpdateProfileEntry mutated a non-scoping column: %+v", e)
	}
}

func TestUpdateProfileEntry_CAS_Conflict(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "sk")
	profileID := createProfile(t, s)
	entryID, ver := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "sk",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: catID,
	})

	_, err := s.UpdateProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: ver - 1, // stale
		EntryId:         entryID,
		Disabled:        true,
	}))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("expected Aborted on CAS conflict, got %v", err)
	}
}

func TestUpdateProfileEntry_OwnerScoped(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "sk")
	profileID := createProfile(t, s)
	entryID, ver := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "sk",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: catID,
	})

	// Bob does not own alice's profile — must not leak existence (NotFound, not PermissionDenied).
	_, err := s.UpdateProfileEntry(bobCtx(), connect.NewRequest(&cpv1.UpdateProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: ver,
		EntryId:         entryID,
		Disabled:        true,
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound for a foreign profile, got %v", err)
	}
}

func TestUpdateProfileEntry_UnknownEntry(t *testing.T) {
	s, _, _ := newTestServer(t)

	profileID := createProfile(t, s)

	_, err := s.UpdateProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: 1,
		EntryId:         "no-such-entry",
		Disabled:        true,
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound for an unknown entry, got %v", err)
	}
}

// --- GetProfile: provenance + disabled ---------------------------------------

func TestGetProfile_ProvenancePopulatedForURLIngestedSkill(t *testing.T) {
	s, _, _ := newTestServer(t)

	sha := "aabbccdd1122334455667788991100aabbccdd1122334455667788991100aabb"
	if err := s.st.CustomizationCatalog().CreateSkill(context.Background(), store.CustomizationCatalogEntry{
		CatalogID:    "url-skill-1",
		CreatorID:    "alice",
		Kind:         string(store.ProfileEntrySkill),
		Name:         "my-url-skill",
		Description:  "ingested from github",
		Listed:       true,
		CreatedAt:    1,
		UpdatedAt:    1,
		SourceURL:    "https://github.com/example/my-skill",
		SourceRef:    "main",
		SourceSubdir: "skills/x",
		SourceCommit: "1111111111111111111111111111111111aaaa",
		SHA256:       &sha,
	}); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	profileID := createProfile(t, s)
	addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "my-url-skill",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: "url-skill-1",
	})

	gr, err := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if len(gr.Msg.Profile.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(gr.Msg.Profile.Entries))
	}
	prov := gr.Msg.Profile.Entries[0].Provenance
	if prov == nil {
		t.Fatal("expected non-nil provenance for a URL-ingested skill")
	}
	if prov.SourceUrl != "https://github.com/example/my-skill" || prov.SourceRef != "main" ||
		prov.SourceSubdir != "skills/x" || prov.SourceCommit != "1111111111111111111111111111111111aaaa" {
		t.Errorf("unexpected provenance: %+v", prov)
	}
}

func TestGetProfile_ProvenanceEmptyForInlineOrCustomEntry(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "inline-sk")
	profileID := createProfile(t, s)
	_, ver := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "inline-sk",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: catID,
	})
	addEntry(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:         "custom-mcp",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: []byte(`{"stdio":{"command":"node"}}`),
	})

	gr, err := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if len(gr.Msg.Profile.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(gr.Msg.Profile.Entries))
	}
	for _, e := range gr.Msg.Profile.Entries {
		if e.Provenance != nil {
			t.Errorf("entry %q (source %v): expected nil provenance, got %+v", e.Name, e.Source, e.Provenance)
		}
	}
}

func TestGetProfile_DisabledRoundTrips(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "sk")
	profileID := createProfile(t, s)
	entryID, ver := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "sk",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: catID,
	})

	gr1, err := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if gr1.Msg.Profile.Entries[0].Disabled {
		t.Error("expected Disabled=false by default")
	}

	if _, err := s.UpdateProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver, EntryId: entryID, Disabled: true,
	})); err != nil {
		t.Fatalf("UpdateProfileEntry: %v", err)
	}

	gr2, err := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if !gr2.Msg.Profile.Entries[0].Disabled {
		t.Error("expected Disabled=true after UpdateProfileEntry")
	}
}

// --- Assembly: disabled entries -----------------------------------------------

func TestAssemble_DisabledEntry_NoArtifactNoPayload(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "sk")
	profileID := createProfile(t, s)
	entryID, ver := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "sk",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: catID,
	})
	if _, err := s.UpdateProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver, EntryId: entryID, Disabled: true,
	})); err != nil {
		t.Fatalf("UpdateProfileEntry: %v", err)
	}

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil specs for an all-disabled profile, got %v", got)
	}
}

func TestAssemble_AllEntriesDisabled_EmptyProfileContract(t *testing.T) {
	s, _, _ := newTestServer(t)

	profileID := createProfile(t, s)
	entryID, ver := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:         "custom-mcp",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: []byte(`{"stdio":{"command":"node"}}`),
	})
	if _, err := s.UpdateProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver, EntryId: entryID, Disabled: true,
	})); err != nil {
		t.Fatalf("UpdateProfileEntry: %v", err)
	}

	p, entries := loadProfile(t, s, profileID)
	gotSpecs, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if gotSpecs != nil {
		t.Errorf("expected (nil, nil) for an all-disabled profile — same contract as an empty profile, got %v", gotSpecs)
	}
}

// TestAssemble_DisabledEntry_NoDuplicateNameCollision proves a disabled entry does not trip the
// duplicate-skill-dir-name check against an enabled entry of the same name — it never reaches
// resolution at all.
func TestAssemble_DisabledEntry_NoDuplicateNameCollision(t *testing.T) {
	s, _, _ := newTestServer(t)

	profileID := createProfile(t, s)
	_, ver := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         "same-name",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: validSkillTar(),
	})
	dupEntryID, ver2 := addEntry(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         "same-name", // duplicate name, but will be disabled
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: validSkillTar(),
	})
	if _, err := s.UpdateProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver2, EntryId: dupEntryID, Disabled: true,
	})); err != nil {
		t.Fatalf("UpdateProfileEntry: %v", err)
	}

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("expected no error (disabled duplicate must not collide), got %v", err)
	}
	if len(got) != 2 { // manifest + 1 skill TAR (the disabled twin emits nothing)
		t.Errorf("want 2 specs (manifest + 1 skill TAR), got %d: %v", len(got), got)
	}
}

// --- Assembly: manifest carries provenance ------------------------------------

func TestAssemble_EnabledURLIngestedSkill_ManifestCarriesSource(t *testing.T) {
	s, _, _ := newTestServer(t)

	sha := "aabbccdd1122334455667788991100aabbccdd1122334455667788991100aabb"
	if err := s.st.CustomizationCatalog().CreateSkill(context.Background(), store.CustomizationCatalogEntry{
		CatalogID:    "url-skill-1",
		CreatorID:    "alice",
		Kind:         string(store.ProfileEntrySkill),
		Name:         "my-url-skill",
		Listed:       true,
		CreatedAt:    1,
		UpdatedAt:    1,
		SourceURL:    "https://github.com/example/my-skill",
		SourceRef:    "main",
		SourceSubdir: "skills/x",
		SourceCommit: "1111111111111111111111111111111111aaaa",
		SHA256:       &sha,
	}); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	profileID := createProfile(t, s)
	addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "my-url-skill",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: "url-skill-1",
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	ms := findManifestSpec(got)
	if ms == nil {
		t.Fatal("no manifest spec in result")
	}
	m := unmarshalManifest(t, ms.Inline)
	if len(m.Artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(m.Artifacts))
	}
	a := m.Artifacts[0]
	if a.Skill == nil || a.Skill.Source == nil {
		t.Fatalf("expected Skill.Source to be set, got %+v", a.Skill)
	}
	if a.Skill.Source.URL != "https://github.com/example/my-skill" || a.Skill.Source.Ref != "main" ||
		a.Skill.Source.Subdir != "skills/x" || a.Skill.Source.Commit != "1111111111111111111111111111111111aaaa" {
		t.Errorf("unexpected Skill.Source: %+v", a.Skill.Source)
	}
}

// TestAssemble_InlineSkill_NoSource proves an inline (custom) skill's manifest artifact carries
// no Skill.Source — operator-authored content is not untrusted-external.
func TestAssemble_InlineSkill_NoSource(t *testing.T) {
	s, _, _ := newTestServer(t)

	profileID := createProfile(t, s)
	addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         "inline-skill",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: validSkillTar(),
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	ms := findManifestSpec(got)
	m := unmarshalManifest(t, ms.Inline)
	if len(m.Artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(m.Artifacts))
	}
	if m.Artifacts[0].Skill == nil {
		t.Fatal("expected Skill payload")
	}
	if m.Artifacts[0].Skill.Source != nil {
		t.Errorf("expected nil Skill.Source for an inline skill, got %+v", m.Artifacts[0].Skill.Source)
	}
}

// TestAssemble_BundleMember_CarriesOwnProvenance proves a bundle member's manifest artifact
// carries THAT member's own catalog row provenance, not the owning bundle entry's.
func TestAssemble_BundleMember_CarriesOwnProvenance(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()

	bundleID := "bnd-prov"
	if err := s.st.SkillBundles().Create(ctx, store.SkillBundle{
		BundleID: bundleID, CreatorID: "alice", Name: "provenance bundle",
		SourceURL: "https://github.com/example/bundle-repo", CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("SkillBundles().Create: %v", err)
	}
	if err := s.st.CustomizationCatalog().CreateSkill(ctx, store.CustomizationCatalogEntry{
		CatalogID: "bnd-prov-cat-0", CreatorID: "alice", Kind: string(store.ProfileEntrySkill),
		Name: "member-skill", Listed: false, CreatedAt: 1000, UpdatedAt: 1000,
		SourceSubdir: "skills/member-skill",
		SourceURL:    "https://github.com/example/bundle-repo", // member's OWN provenance
		SourceCommit: "2222222222222222222222222222222222222b",
		SHA256:       ptrStr("aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa1"),
		BundleMember: true,
	}); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	versionID := "bndv-prov"
	if err := s.st.SkillBundles().CreateVersion(ctx, store.SkillBundleVersion{
		VersionID: versionID, BundleID: bundleID, Seq: 1, CreatedAt: 1000,
	}, []store.SkillBundleMember{{SourceSubdir: "skills/member-skill", CatalogID: "bnd-prov-cat-0", Position: 0}}); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	profileID := createProfile(t, s)
	addBundleRefEntry(t, s, profileID, 1, "ent-bnd-prov", "my-bundle", bundleID, versionID, nil)

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(ctx, p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	ms := findManifestSpec(got)
	m := unmarshalManifest(t, ms.Inline)
	if len(m.Artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(m.Artifacts))
	}
	a := m.Artifacts[0]
	if a.Skill == nil || a.Skill.Source == nil {
		t.Fatalf("expected Skill.Source to be set, got %+v", a.Skill)
	}
	if a.Skill.Source.URL != "https://github.com/example/bundle-repo" || a.Skill.Source.Commit != "2222222222222222222222222222222222222b" {
		t.Errorf("unexpected Skill.Source: %+v", a.Skill.Source)
	}
}

func ptrStr(s string) *string { return &s }
