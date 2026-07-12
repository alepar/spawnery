package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
)

// TestAssembleBundleRefArtifactsCarryBundleMark is the hermetic guard for the sp-mwco.2.9 fix: a
// profile with one bundle_ref entry (2 members) plus a standalone catalog_ref skill assembles such
// that every bundle member's manifest Artifact.Bundle equals the owning bundle_ref entry's EntryID
// (spec.Artifact.Bundle's doc comment: "members of the same bundle_ref share the same Bundle
// value"), and the standalone artifact's Bundle is empty. Before this fix, buildManifestAndPayloads
// never set Artifact.Bundle at all, so manifestHasBundle() was always false and the all-or-nothing
// contract (EvaluateApplyReport / BuildApplyReport rollups) was silently inert in production.
func TestAssembleBundleRefArtifactsCarryBundleMark(t *testing.T) {
	s, _, _ := newTestServer(t)

	bundleID, versionID, _, names := seedBundle(t, s, "alice", 2)

	catResp, err := s.CreateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.CreateCatalogEntryRequest{
		Kind:    cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:    "standalone-bundlemark-skill",
		Content: validSkillTar(),
	}))
	if err != nil {
		t.Fatalf("CreateCatalogEntry: %v", err)
	}

	profileID := createProfile(t, s)
	ver := addBundleRefEntry(t, s, profileID, 1, "ent-bundle-mark", "my-bundle", bundleID, versionID, nil)
	_, _ = addEntry(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "standalone-bundlemark-skill",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: catResp.Msg.CatalogId,
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}

	ms := findManifestSpec(got)
	if ms == nil {
		t.Fatal("no manifest spec")
	}
	m := unmarshalManifest(t, ms.Inline)

	for _, name := range names {
		a := findArtifactByName(m, name)
		if a == nil {
			t.Fatalf("no manifest artifact named %q", name)
		}
		if a.Bundle != "ent-bundle-mark" {
			t.Errorf("member %q: Bundle = %q, want %q", name, a.Bundle, "ent-bundle-mark")
		}
	}

	standalone := findArtifactByName(m, "standalone-bundlemark-skill")
	if standalone == nil {
		t.Fatal("no manifest artifact for standalone entry")
	}
	if standalone.Bundle != "" {
		t.Errorf("standalone entry: Bundle = %q, want empty", standalone.Bundle)
	}
}
