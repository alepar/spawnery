package store_test

import (
	"context"
	"errors"
	"testing"

	"spawnery/internal/cp/store"
)

func TestProfiles_CreateGet(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	p := store.Profile{
		ProfileID: "pf-1",
		OwnerID:   "alice",
		Name:      "My Profile",
		Version:   1,
		UpdatedAt: 1000,
	}
	if err := st.Profiles().Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, entries, secrets, err := st.Profiles().Get(ctx, "pf-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProfileID != "pf-1" || got.OwnerID != "alice" || got.Name != "My Profile" {
		t.Errorf("unexpected profile: %+v", got)
	}
	if got.Version != 1 {
		t.Errorf("expected version 1, got %d", got.Version)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
	if len(secrets) != 0 {
		t.Errorf("expected 0 secrets, got %d", len(secrets))
	}
}

func TestProfiles_GetNotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	_, _, _, err := st.Profiles().Get(ctx, "nonexistent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestProfiles_ListByOwner(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	for i, id := range []string{"pf-a1", "pf-a2", "pf-b1"} {
		owner := "alice"
		if i == 2 {
			owner = "bob"
		}
		if err := st.Profiles().Create(ctx, store.Profile{
			ProfileID: id, OwnerID: owner, Name: id, Version: 1, UpdatedAt: 1000,
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	aliceProfiles, err := st.Profiles().ListByOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("ListByOwner alice: %v", err)
	}
	if len(aliceProfiles) != 2 {
		t.Errorf("expected 2 alice profiles, got %d", len(aliceProfiles))
	}

	bobProfiles, err := st.Profiles().ListByOwner(ctx, "bob")
	if err != nil {
		t.Fatalf("ListByOwner bob: %v", err)
	}
	if len(bobProfiles) != 1 {
		t.Errorf("expected 1 bob profile, got %d", len(bobProfiles))
	}

	noneProfiles, err := st.Profiles().ListByOwner(ctx, "carol")
	if err != nil {
		t.Fatalf("ListByOwner carol: %v", err)
	}
	if len(noneProfiles) != 0 {
		t.Errorf("expected 0 carol profiles, got %d", len(noneProfiles))
	}
}

func TestProfiles_Rename(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-r1", OwnerID: "alice", Name: "Original", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	newVer, err := st.Profiles().Rename(ctx, "pf-r1", 1, "Renamed", 2000)
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if newVer != 2 {
		t.Errorf("expected version 2, got %d", newVer)
	}

	got, _, _, err := st.Profiles().Get(ctx, "pf-r1")
	if err != nil {
		t.Fatalf("Get after Rename: %v", err)
	}
	if got.Name != "Renamed" {
		t.Errorf("expected name 'Renamed', got %q", got.Name)
	}
	if got.Version != 2 {
		t.Errorf("expected version 2, got %d", got.Version)
	}
	if got.UpdatedAt != 2000 {
		t.Errorf("expected updated_at 2000, got %d", got.UpdatedAt)
	}
}

func TestProfiles_Rename_StaleVersion(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-stale", OwnerID: "alice", Name: "Orig", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance version via rename.
	if _, err := st.Profiles().Rename(ctx, "pf-stale", 1, "First rename", 2000); err != nil {
		t.Fatalf("first Rename: %v", err)
	}

	// Now try with the old expected version → ErrConflict.
	_, err := st.Profiles().Rename(ctx, "pf-stale", 1, "Bad rename", 3000)
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict on stale version, got %v", err)
	}
}

func TestProfiles_Rename_NotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	_, err := st.Profiles().Rename(ctx, "no-such", 1, "x", 1000)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestProfiles_AddEntry(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-e1", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	entry := store.ProfileEntry{
		ProfileID:  "pf-e1",
		EntryID:    "ent-1",
		Kind:       store.ProfileEntrySkill,
		Name:       "my-skill",
		SourceKind: store.ProfileSourceCatalog,
		CatalogID:  "alice/myskill",
		Targets:    []string{"all"},
	}
	newVer, err := st.Profiles().AddEntry(ctx, "pf-e1", 1, entry, 2000)
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	if newVer != 2 {
		t.Errorf("expected version 2, got %d", newVer)
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-e1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.EntryID != "ent-1" || e.Kind != store.ProfileEntrySkill || e.Name != "my-skill" {
		t.Errorf("unexpected entry: %+v", e)
	}
	if len(e.Targets) != 1 || e.Targets[0] != "all" {
		t.Errorf("unexpected targets: %v", e.Targets)
	}
}

func TestProfiles_AddEntry_DefaultTargets(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-dt", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Entry with empty Targets → should default to ["all"].
	entry := store.ProfileEntry{
		ProfileID:  "pf-dt",
		EntryID:    "ent-dt",
		Kind:       store.ProfileEntryMCP,
		Name:       "mcp-tool",
		SourceKind: store.ProfileSourceCatalog,
		CatalogID:  "alice/mcp",
		Targets:    nil, // empty → default "all"
	}
	if _, err := st.Profiles().AddEntry(ctx, "pf-dt", 1, entry, 2000); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-dt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if len(entries[0].Targets) != 1 || entries[0].Targets[0] != "all" {
		t.Errorf("expected default targets=[all], got %v", entries[0].Targets)
	}
}

func TestProfiles_AddEntry_CustomInlineAndSecretRefs(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-ci", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	entry := store.ProfileEntry{
		ProfileID:     "pf-ci",
		EntryID:       "ent-ci",
		Kind:          store.ProfileEntryMCP,
		Name:          "custom-mcp",
		SourceKind:    store.ProfileSourceCustom,
		CustomInline:  []byte(`{"command":"docker","args":["run","my-mcp"]}`),
		Targets:       []string{"goose"},
		MCPSecretRefs: []string{"MY_API_KEY", "ANOTHER_KEY"},
	}
	if _, err := st.Profiles().AddEntry(ctx, "pf-ci", 1, entry, 2000); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-ci")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if string(e.CustomInline) != `{"command":"docker","args":["run","my-mcp"]}` {
		t.Errorf("unexpected custom_inline: %s", e.CustomInline)
	}
	if len(e.MCPSecretRefs) != 2 || e.MCPSecretRefs[0] != "MY_API_KEY" {
		t.Errorf("unexpected secret refs: %v", e.MCPSecretRefs)
	}
}

func TestProfiles_RemoveEntry(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-re", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	entry := store.ProfileEntry{
		ProfileID: "pf-re", EntryID: "ent-1",
		Kind: store.ProfileEntrySkill, Name: "sk", SourceKind: store.ProfileSourceCatalog, CatalogID: "x/y",
	}
	newVer, err := st.Profiles().AddEntry(ctx, "pf-re", 1, entry, 2000)
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	finalVer, err := st.Profiles().RemoveEntry(ctx, "pf-re", newVer, "ent-1", 3000)
	if err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if finalVer != 3 {
		t.Errorf("expected version 3, got %d", finalVer)
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-re")
	if err != nil {
		t.Fatalf("Get after RemoveEntry: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after remove, got %d", len(entries))
	}
}

func TestProfiles_AddSecretRef(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-s1", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	newVer, err := st.Profiles().AddSecretRef(ctx, "pf-s1", 1, "sec-abc", 2000)
	if err != nil {
		t.Fatalf("AddSecretRef: %v", err)
	}
	if newVer != 2 {
		t.Errorf("expected version 2, got %d", newVer)
	}

	_, _, secrets, err := st.Profiles().Get(ctx, "pf-s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(secrets) != 1 || secrets[0].SecretID != "sec-abc" {
		t.Errorf("unexpected secrets: %v", secrets)
	}
}

func TestProfiles_RemoveSecretRef(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-rs", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ver2, err := st.Profiles().AddSecretRef(ctx, "pf-rs", 1, "sec-xyz", 2000)
	if err != nil {
		t.Fatalf("AddSecretRef: %v", err)
	}

	finalVer, err := st.Profiles().RemoveSecretRef(ctx, "pf-rs", ver2, "sec-xyz", 3000)
	if err != nil {
		t.Fatalf("RemoveSecretRef: %v", err)
	}
	if finalVer != 3 {
		t.Errorf("expected version 3, got %d", finalVer)
	}

	_, _, secrets, err := st.Profiles().Get(ctx, "pf-rs")
	if err != nil {
		t.Fatalf("Get after RemoveSecretRef: %v", err)
	}
	if len(secrets) != 0 {
		t.Errorf("expected 0 secrets after remove, got %d", len(secrets))
	}
}

func TestProfiles_CAS_Conflict(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-cas", OwnerID: "alice", Name: "CAS Test", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance to version 2.
	if _, err := st.Profiles().Rename(ctx, "pf-cas", 1, "v2", 2000); err != nil {
		t.Fatalf("Rename to v2: %v", err)
	}

	// Stale CAS on AddEntry.
	entry := store.ProfileEntry{
		ProfileID: "pf-cas", EntryID: "ent-x",
		Kind: store.ProfileEntrySkill, Name: "x", SourceKind: store.ProfileSourceCatalog, CatalogID: "a/b",
	}
	_, err := st.Profiles().AddEntry(ctx, "pf-cas", 1 /* stale */, entry, 3000)
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict on stale AddEntry, got %v", err)
	}

	// Stale CAS on RemoveEntry (need a valid entry first).
	if _, err := st.Profiles().AddEntry(ctx, "pf-cas", 2, entry, 3000); err != nil {
		t.Fatalf("AddEntry at v2: %v", err)
	}
	// Now at version 3; try stale RemoveEntry.
	_, err = st.Profiles().RemoveEntry(ctx, "pf-cas", 2 /* stale */, "ent-x", 4000)
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict on stale RemoveEntry, got %v", err)
	}

	// Stale CAS on AddSecretRef.
	_, err = st.Profiles().AddSecretRef(ctx, "pf-cas", 2 /* stale */, "sec-1", 4000)
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict on stale AddSecretRef, got %v", err)
	}
}

func TestProfiles_Delete(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-del", OwnerID: "alice", Name: "To Delete", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Add an entry and a secret ref so we verify cascading delete.
	ver2, err := st.Profiles().AddEntry(ctx, "pf-del", 1, store.ProfileEntry{
		ProfileID: "pf-del", EntryID: "ent-del",
		Kind: store.ProfileEntrySkill, Name: "sk", SourceKind: store.ProfileSourceCatalog, CatalogID: "a/b",
	}, 2000)
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	if _, err := st.Profiles().AddSecretRef(ctx, "pf-del", ver2, "sec-del", 3000); err != nil {
		t.Fatalf("AddSecretRef: %v", err)
	}

	if err := st.Profiles().Delete(ctx, "pf-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, _, _, err = st.Profiles().Get(ctx, "pf-del")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound after Delete, got %v", err)
	}
}

func TestProfiles_EntriesOrderedByEntryID(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-ord", OwnerID: "alice", Name: "Order", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Insert in reverse alphabetical order.
	ids := []string{"ent-z", "ent-a", "ent-m"}
	ver := uint64(1)
	for _, id := range ids {
		var err error
		ver, err = st.Profiles().AddEntry(ctx, "pf-ord", ver, store.ProfileEntry{
			ProfileID: "pf-ord", EntryID: id,
			Kind: store.ProfileEntrySkill, Name: id, SourceKind: store.ProfileSourceCatalog, CatalogID: "a/b",
		}, 2000)
		if err != nil {
			t.Fatalf("AddEntry %s: %v", id, err)
		}
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-ord")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	// Should be sorted ASC.
	expected := []string{"ent-a", "ent-m", "ent-z"}
	for i, want := range expected {
		if entries[i].EntryID != want {
			t.Errorf("entry[%d]: got %q, want %q", i, entries[i].EntryID, want)
		}
	}
}

// TestProfileEntryBundleRefRoundTrip proves fact 4 from the sp-mwco.1.5 plan: catalog_id is
// already NOT NULL DEFAULT ” (sp-mwco.1.3), so a bundle_ref entry that leaves CatalogID empty
// and sets BundleID/VersionID instead round-trips cleanly with no NOT NULL violation.
func TestProfileEntryBundleRefRoundTrip(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-br", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	entry := store.ProfileEntry{
		ProfileID:  "pf-br",
		EntryID:    "ent-br",
		Kind:       store.ProfileEntrySkill,
		Name:       "some-bundle",
		SourceKind: store.ProfileSourceBundle,
		BundleID:   "bnd-1",
		VersionID:  "bndv-1",
		Targets:    []string{"all"},
	}
	if _, err := st.Profiles().AddEntry(ctx, "pf-br", 1, entry, 2000); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-br")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.SourceKind != store.ProfileSourceBundle {
		t.Errorf("SourceKind = %q, want %q", e.SourceKind, store.ProfileSourceBundle)
	}
	if e.BundleID != "bnd-1" || e.VersionID != "bndv-1" {
		t.Errorf("BundleID/VersionID = %q/%q, want bnd-1/bndv-1", e.BundleID, e.VersionID)
	}
	if e.CatalogID != "" {
		t.Errorf("CatalogID = %q, want empty for bundle_ref entry", e.CatalogID)
	}
}

// TestProfileEntryBundleOverridesRoundTrip proves a bundle_ref entry's exclude/rename overrides
// survive AddEntry -> Get (sp-mwco.1.8).
func TestProfileEntryBundleOverridesRoundTrip(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-bo", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	entry := store.ProfileEntry{
		ProfileID:      "pf-bo",
		EntryID:        "ent-bo",
		Kind:           store.ProfileEntrySkill,
		Name:           "my-bundle",
		SourceKind:     store.ProfileSourceBundle,
		BundleID:       "bnd-1",
		VersionID:      "bndv-1",
		ExcludeSubdirs: []string{"skills/foo"},
		RenameSubdirs:  map[string]string{"skills/bar": "bar-2"},
	}
	if _, err := st.Profiles().AddEntry(ctx, "pf-bo", 1, entry, 2000); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-bo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if len(e.ExcludeSubdirs) != 1 || e.ExcludeSubdirs[0] != "skills/foo" {
		t.Errorf("ExcludeSubdirs = %v, want [skills/foo]", e.ExcludeSubdirs)
	}
	if len(e.RenameSubdirs) != 1 || e.RenameSubdirs["skills/bar"] != "bar-2" {
		t.Errorf("RenameSubdirs = %v, want {skills/bar: bar-2}", e.RenameSubdirs)
	}
}

// TestProfileEntryBundleOverridesEmptyDecodesNil proves an entry with no overrides (empty
// bundle_overrides column) decodes to nil/empty slices/maps, not zero-length-but-non-nil noise.
func TestProfileEntryBundleOverridesEmptyDecodesNil(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-bo2", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	entry := store.ProfileEntry{
		ProfileID:  "pf-bo2",
		EntryID:    "ent-bo2",
		Kind:       store.ProfileEntrySkill,
		Name:       "my-bundle",
		SourceKind: store.ProfileSourceBundle,
		BundleID:   "bnd-1",
		VersionID:  "bndv-1",
	}
	if _, err := st.Profiles().AddEntry(ctx, "pf-bo2", 1, entry, 2000); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-bo2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if len(e.ExcludeSubdirs) != 0 {
		t.Errorf("ExcludeSubdirs = %v, want empty", e.ExcludeSubdirs)
	}
	if len(e.RenameSubdirs) != 0 {
		t.Errorf("RenameSubdirs = %v, want empty", e.RenameSubdirs)
	}
}

// TestProfiles_UpdateEntryPin proves UpdateEntryPin bumps the CAS version and rewrites the
// entry's version_id + overrides (sp-mwco.1.8 — RepinProfileBundle's store call).
func TestProfiles_UpdateEntryPin(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-pin", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ver, err := st.Profiles().AddEntry(ctx, "pf-pin", 1, store.ProfileEntry{
		ProfileID: "pf-pin", EntryID: "ent-pin", Kind: store.ProfileEntrySkill, Name: "my-bundle",
		SourceKind: store.ProfileSourceBundle, BundleID: "bnd-1", VersionID: "bndv-1",
		ExcludeSubdirs: []string{"skills/foo"},
	}, 2000)
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	overridesJSON, err := store.EncodeBundleOverrides(nil, map[string]string{"skills/bar": "bar-2"})
	if err != nil {
		t.Fatalf("EncodeBundleOverrides: %v", err)
	}
	newVer, err := st.Profiles().UpdateEntryPin(ctx, "pf-pin", ver, "ent-pin", "bndv-2", overridesJSON, 3000)
	if err != nil {
		t.Fatalf("UpdateEntryPin: %v", err)
	}
	if newVer != ver+1 {
		t.Errorf("newVer = %d, want %d", newVer, ver+1)
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-pin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.VersionID != "bndv-2" {
		t.Errorf("VersionID = %q, want bndv-2", e.VersionID)
	}
	if len(e.ExcludeSubdirs) != 0 {
		t.Errorf("ExcludeSubdirs = %v, want empty (overwritten by the new overrides)", e.ExcludeSubdirs)
	}
	if len(e.RenameSubdirs) != 1 || e.RenameSubdirs["skills/bar"] != "bar-2" {
		t.Errorf("RenameSubdirs = %v, want {skills/bar: bar-2}", e.RenameSubdirs)
	}
}

// TestProfiles_UpdateEntryPin_StaleVersion proves a stale expected_version is rejected with
// ErrConflict, mirroring AddEntry/RemoveEntry's CAS behavior.
func TestProfiles_UpdateEntryPin_StaleVersion(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-pinstale", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.Profiles().AddEntry(ctx, "pf-pinstale", 1, store.ProfileEntry{
		ProfileID: "pf-pinstale", EntryID: "ent-pin", Kind: store.ProfileEntrySkill, Name: "my-bundle",
		SourceKind: store.ProfileSourceBundle, BundleID: "bnd-1", VersionID: "bndv-1",
	}, 2000); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	_, err := st.Profiles().UpdateEntryPin(ctx, "pf-pinstale", 1 /* stale, now 2 */, "ent-pin", "bndv-2", "", 3000)
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict on stale UpdateEntryPin, got %v", err)
	}
}

// TestProfiles_UpdateEntryPin_UnknownEntry proves repinning an entry that does not exist on an
// otherwise-valid profile returns ErrNotFound, not a silent no-op.
func TestProfiles_UpdateEntryPin_UnknownEntry(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-pinnf", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := st.Profiles().UpdateEntryPin(ctx, "pf-pinnf", 1, "no-such-entry", "bndv-2", "", 2000)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown entry, got %v", err)
	}
}

// TestProfiles_UpdateEntry proves UpdateEntry changes targets AND disabled in place (same
// entry_id), bumps the profile version, and leaves the entry's other columns
// (kind/name/source/catalog_id) untouched (sp-mwco.2.8 §4.6).
func TestProfiles_UpdateEntry(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-ue", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ver, err := st.Profiles().AddEntry(ctx, "pf-ue", 1, store.ProfileEntry{
		ProfileID: "pf-ue", EntryID: "ent-ue", Kind: store.ProfileEntrySkill, Name: "my-skill",
		SourceKind: store.ProfileSourceCatalog, CatalogID: "alice/myskill", Targets: []string{"claude"},
	}, 2000)
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	newVer, err := st.Profiles().UpdateEntry(ctx, "pf-ue", ver, "ent-ue", []string{"claude", "goose"}, true, 3000)
	if err != nil {
		t.Fatalf("UpdateEntry: %v", err)
	}
	if newVer != ver+1 {
		t.Errorf("newVer = %d, want %d", newVer, ver+1)
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-ue")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if !e.Disabled {
		t.Errorf("expected Disabled=true after UpdateEntry")
	}
	if len(e.Targets) != 2 || e.Targets[0] != "claude" || e.Targets[1] != "goose" {
		t.Errorf("Targets = %v, want [claude goose]", e.Targets)
	}
	// Other columns untouched.
	if e.Kind != store.ProfileEntrySkill || e.Name != "my-skill" ||
		e.SourceKind != store.ProfileSourceCatalog || e.CatalogID != "alice/myskill" {
		t.Errorf("UpdateEntry mutated a non-scoping column: %+v", e)
	}
}

// TestProfiles_UpdateEntry_DefaultDisabledFalse proves AddEntry+Get round-trips Disabled=false
// by default (a fresh entry is enabled).
func TestProfiles_UpdateEntry_DefaultDisabledFalse(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-ued", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.Profiles().AddEntry(ctx, "pf-ued", 1, store.ProfileEntry{
		ProfileID: "pf-ued", EntryID: "ent-1", Kind: store.ProfileEntrySkill, Name: "sk",
		SourceKind: store.ProfileSourceCatalog, CatalogID: "x/y",
	}, 2000); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	_, entries, _, err := st.Profiles().Get(ctx, "pf-ued")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 || entries[0].Disabled {
		t.Fatalf("expected Disabled=false by default, got %+v", entries)
	}
}

// TestProfiles_UpdateEntry_StaleVersion proves a stale expected_version is rejected with
// ErrConflict, mirroring UpdateEntryPin's CAS behavior.
func TestProfiles_UpdateEntry_StaleVersion(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-uestale", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.Profiles().AddEntry(ctx, "pf-uestale", 1, store.ProfileEntry{
		ProfileID: "pf-uestale", EntryID: "ent-1", Kind: store.ProfileEntrySkill, Name: "sk",
		SourceKind: store.ProfileSourceCatalog, CatalogID: "x/y",
	}, 2000); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	_, err := st.Profiles().UpdateEntry(ctx, "pf-uestale", 1 /* stale, now 2 */, "ent-1", nil, true, 3000)
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict on stale UpdateEntry, got %v", err)
	}
}

// TestProfiles_UpdateEntry_UnknownEntry proves updating an entry that does not exist on an
// otherwise-valid profile returns ErrNotFound, not a silent no-op.
func TestProfiles_UpdateEntry_UnknownEntry(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-uenf", OwnerID: "alice", Name: "Profile", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := st.Profiles().UpdateEntry(ctx, "pf-uenf", 1, "no-such-entry", nil, false, 2000)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown entry, got %v", err)
	}
}

func TestProfiles_CountRefsByCatalogRef(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	// bob has two profiles referencing cat-1; carol has one. A fourth (decoy) profile
	// references a different catalog entry (cat-2) and must not be counted.
	profiles := []struct {
		id, owner, catalogRef string
	}{
		{"pf-1", "bob", "cat-1"},
		{"pf-2", "bob", "cat-1"},
		{"pf-3", "carol", "cat-1"},
		{"pf-4", "dave", "cat-2"},
	}
	for _, p := range profiles {
		if err := st.Profiles().Create(ctx, store.Profile{
			ProfileID: p.id, OwnerID: p.owner, Name: "p", Version: 1, UpdatedAt: 1000,
		}); err != nil {
			t.Fatalf("Create %s: %v", p.id, err)
		}
		if _, err := st.Profiles().AddEntry(ctx, p.id, 1, store.ProfileEntry{
			EntryID: "e-" + p.id, Kind: store.ProfileEntrySkill, Name: "sk",
			SourceKind: store.ProfileSourceCatalog, CatalogID: p.catalogRef,
		}, 2000); err != nil {
			t.Fatalf("AddEntry %s: %v", p.id, err)
		}
	}

	gotProfiles, gotOwners, err := st.Profiles().CountRefsByCatalogRef(ctx, "cat-1")
	if err != nil {
		t.Fatalf("CountRefsByCatalogRef: %v", err)
	}
	if gotProfiles != 3 {
		t.Errorf("expected 3 referencing profiles, got %d", gotProfiles)
	}
	if gotOwners != 2 {
		t.Errorf("expected 2 distinct owners, got %d", gotOwners)
	}
}

func TestProfiles_CountRefsByCatalogRef_NoRefs(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	gotProfiles, gotOwners, err := st.Profiles().CountRefsByCatalogRef(ctx, "no-such-catalog-id")
	if err != nil {
		t.Fatalf("CountRefsByCatalogRef: %v", err)
	}
	if gotProfiles != 0 || gotOwners != 0 {
		t.Errorf("expected (0, 0), got (%d, %d)", gotProfiles, gotOwners)
	}
}
