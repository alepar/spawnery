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
