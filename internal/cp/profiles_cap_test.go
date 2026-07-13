package cp

import (
	"fmt"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
)

// This file holds sp-mwco.1.12's RPC-level tests: AddProfileEntry's catalog_ref/custom branch
// must account for a profile's EXPANDED artifact count — a bundle_ref entry contributes its
// post-exclude member count, not 1 — so a catalog_ref/custom attach can't push a bundle-bearing
// profile past the 63-payload budget and make every CreateSpawn hard-fail (§4.4). seedBundle
// (profiles_assembly_bundle_test.go), createProfile/addEntry/addEntryReturningVer/validSkillTar
// (profiles_assembly_test.go, profiles_bundle_test.go) and aliceCtx (profiles_test.go) are reused.

// TestAddProfileEntry_Custom_CountsBundleMembersAgainstCap proves the bug: a 63-member bundle
// already fills the expanded-artifact budget, so a subsequent custom attach must be rejected —
// before the fix, the custom branch only counted ENTRIES (1 bundle_ref entry < 64) and let this
// through, producing a profile that would fail every CreateSpawn at assembly.
func TestAddProfileEntry_Custom_CountsBundleMembersAgainstCap(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, _ := seedBundle(t, s, "alice", 63)
	profileID := createProfile(t, s)

	ver := addEntryReturningVer(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "my-bundle",
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
	})

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "one-more",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument over the expanded artifact cap, got %v", err)
	}
}

// TestAddProfileEntry_CatalogRef_CountsBundleMembersAgainstCap is the CATALOG_REF-source twin of
// the above: the accounting bug is in the shared "else" (non-bundle) branch, so both sources must
// be covered.
func TestAddProfileEntry_CatalogRef_CountsBundleMembersAgainstCap(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, _ := seedBundle(t, s, "alice", 63)
	profileID := createProfile(t, s)
	catalogID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "alice-visible-skill")

	ver := addEntryReturningVer(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "my-bundle",
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
	})

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "one-more",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF, CatalogId: catalogID,
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument over the expanded artifact cap, got %v", err)
	}
}

// TestAddProfileEntry_Custom_BudgetBoundary proves the 62+1=63 boundary: a 62-member bundle plus
// one custom entry lands exactly at budget (63) and must succeed; a second custom entry (64) must
// be rejected.
func TestAddProfileEntry_Custom_BudgetBoundary(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, _ := seedBundle(t, s, "alice", 62)
	profileID := createProfile(t, s)

	ver := addEntryReturningVer(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "my-bundle",
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
	})

	ver = addEntryReturningVer(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "custom-1",
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
	})

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "custom-2",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument at 64 total, got %v", err)
	}
}

// TestAddProfileEntry_Custom_NoBundle_Budget63 locks the off-by-one fix on a bundle-free profile:
// 63 plain custom entries fit the budget exactly; the 64th must be rejected. Before the fix,
// enforceProfileEntryCap allowed 64 entries (65 payloads including manifest.json), which assembly
// would then reject.
func TestAddProfileEntry_Custom_NoBundle_Budget63(t *testing.T) {
	s, _, _ := newTestServer(t)
	profileID := createProfile(t, s)

	ver := uint64(1)
	for i := 0; i < 63; i++ {
		ver = addEntryReturningVer(t, s, profileID, ver, &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "custom",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
		})
	}

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "custom-64",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for the 64th entry, got %v", err)
	}
}

// TestAddProfileEntry_ExcludedMembersDoNotCount proves the count is post-exclude, not raw member
// count: a 63-member bundle attached with 2 excluded_subdirs expands to 61, leaving room for 2
// more custom entries (63 total) — both must succeed; a 3rd must be rejected.
func TestAddProfileEntry_ExcludedMembersDoNotCount(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, names := seedBundle(t, s, "alice", 63)
	profileID := createProfile(t, s)

	excluded := []string{"skills/" + names[0], "skills/" + names[1]}
	ver := addEntryReturningVer(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "my-bundle",
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
		ExcludedSubdirs: excluded,
	})

	ver = addEntryReturningVer(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "custom-1",
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
	})
	ver = addEntryReturningVer(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "custom-2",
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
	})

	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "custom-3",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for the 3rd custom entry (63 excluded-bundle + 2 custom already at budget), got %v", err)
	}
}

// TestAddProfileEntry_AtCapProfileStillAssembles is the attach-cap <-> assembly-cap agreement
// test: a profile built to exactly 63 expanded artifacts via AddProfileEntry must assemble to 64
// ArtifactSpecs (63 payloads + manifest.json) with no error — proving the two caps are provably
// consistent, not coincidentally so.
func TestAddProfileEntry_AtCapProfileStillAssembles(t *testing.T) {
	s, _, _ := newTestServer(t)
	bundleID, _, _, _ := seedBundle(t, s, "alice", 60)
	profileID := createProfile(t, s)

	ver := addEntryReturningVer(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "my-bundle",
		Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF, BundleId: bundleID,
	})
	for i := 0; i < 3; i++ {
		ver = addEntryReturningVer(t, s, profileID, ver, &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: fmt.Sprintf("custom-%d", i),
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
		})
	}

	// One more entry should now be rejected — confirms the profile is genuinely AT the 63 budget.
	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId: profileID, ExpectedVersion: ver,
		Entry: &cpv1.ProfileEntry{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "one-too-many",
			Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM, CustomInline: validSkillTar(),
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument at 64 total (sanity check that we're at the 63 budget), got %v", err)
	}

	profile, entries := loadProfile(t, s, profileID)
	specs, err := s.assembleProfileArtifacts(aliceCtx(), profile, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	got, err := validateAndMergeArtifacts(nil, specs)
	if err != nil {
		t.Fatalf("validateAndMergeArtifacts: %v", err)
	}
	if len(got) != 64 {
		t.Errorf("expected 64 artifacts (63 payloads + manifest.json), got %d", len(got))
	}
}
