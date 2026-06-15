package store_test

import (
	"context"
	"errors"
	"testing"

	"spawnery/internal/cp/store"
)

func makeEntry(catalogID, creatorID, name string) store.CustomizationCatalogEntry {
	return store.CustomizationCatalogEntry{
		CatalogID:   catalogID,
		CreatorID:   creatorID,
		Kind:        string(store.ProfileEntrySkill),
		Name:        name,
		Description: "a test entry",
		Content:     []byte("content"),
		Listed:      true,
		CreatedAt:   1000,
		UpdatedAt:   1000,
	}
}

func TestCustomizationCatalog_CreateGet(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	e := makeEntry("cat-1", "alice", "my-skill")
	if err := st.CustomizationCatalog().Create(ctx, e); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := st.CustomizationCatalog().Get(ctx, "cat-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CatalogID != "cat-1" || got.CreatorID != "alice" || got.Name != "my-skill" {
		t.Errorf("unexpected entry: %+v", got)
	}
	if !got.Listed {
		t.Error("expected listed=true")
	}
}

func TestCustomizationCatalog_GetNotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	_, err := st.CustomizationCatalog().Get(ctx, "no-such")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestCustomizationCatalog_List_ListedOnly(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	listed := makeEntry("cat-listed", "alice", "listed-skill")
	listed.Listed = true
	unlisted := makeEntry("cat-unlisted", "alice", "unlisted-skill")
	unlisted.Listed = false

	if err := st.CustomizationCatalog().Create(ctx, listed); err != nil {
		t.Fatalf("Create listed: %v", err)
	}
	if err := st.CustomizationCatalog().Create(ctx, unlisted); err != nil {
		t.Fatalf("Create unlisted: %v", err)
	}

	all, err := st.CustomizationCatalog().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 listed entry, got %d: %v", len(all), all)
	}
	if all[0].CatalogID != "cat-listed" {
		t.Errorf("expected cat-listed, got %q", all[0].CatalogID)
	}
}

func TestCustomizationCatalog_List_OrderedByName(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"zebra", "alpha", "mango"} {
		e := makeEntry("cat-"+name, "alice", name)
		if err := st.CustomizationCatalog().Create(ctx, e); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	all, err := st.CustomizationCatalog().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(all))
	}
	want := []string{"alpha", "mango", "zebra"}
	for i, w := range want {
		if all[i].Name != w {
			t.Errorf("entry[%d]: got %q, want %q", i, all[i].Name, w)
		}
	}
}

func TestCustomizationCatalog_ListByCreator(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	aliceListed := makeEntry("cat-al", "alice", "alice-listed")
	aliceListed.Listed = true
	aliceUnlisted := makeEntry("cat-au", "alice", "alice-unlisted")
	aliceUnlisted.Listed = false
	bobEntry := makeEntry("cat-b", "bob", "bob-skill")

	for _, e := range []store.CustomizationCatalogEntry{aliceListed, aliceUnlisted, bobEntry} {
		if err := st.CustomizationCatalog().Create(ctx, e); err != nil {
			t.Fatalf("Create %s: %v", e.CatalogID, err)
		}
	}

	// Alice's list includes unlisted entries.
	aliceEntries, err := st.CustomizationCatalog().ListByCreator(ctx, "alice")
	if err != nil {
		t.Fatalf("ListByCreator alice: %v", err)
	}
	if len(aliceEntries) != 2 {
		t.Errorf("expected 2 alice entries, got %d", len(aliceEntries))
	}

	bobEntries, err := st.CustomizationCatalog().ListByCreator(ctx, "bob")
	if err != nil {
		t.Fatalf("ListByCreator bob: %v", err)
	}
	if len(bobEntries) != 1 {
		t.Errorf("expected 1 bob entry, got %d", len(bobEntries))
	}

	noneEntries, err := st.CustomizationCatalog().ListByCreator(ctx, "carol")
	if err != nil {
		t.Fatalf("ListByCreator carol: %v", err)
	}
	if len(noneEntries) != 0 {
		t.Errorf("expected 0 carol entries, got %d", len(noneEntries))
	}
}

func TestCustomizationCatalog_Update(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.CustomizationCatalog().Create(ctx, makeEntry("cat-upd", "alice", "original")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := st.CustomizationCatalog().Update(ctx, "cat-upd", "renamed", "new desc", []byte("new content"), 2000); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := st.CustomizationCatalog().Get(ctx, "cat-upd")
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if got.Name != "renamed" {
		t.Errorf("expected name 'renamed', got %q", got.Name)
	}
	if got.Description != "new desc" {
		t.Errorf("expected description 'new desc', got %q", got.Description)
	}
	if string(got.Content) != "new content" {
		t.Errorf("expected content 'new content', got %q", got.Content)
	}
	if got.UpdatedAt != 2000 {
		t.Errorf("expected updated_at 2000, got %d", got.UpdatedAt)
	}
}

func TestCustomizationCatalog_Update_NotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	err := st.CustomizationCatalog().Update(ctx, "no-such", "x", "d", nil, 1000)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestCustomizationCatalog_SetListed(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.CustomizationCatalog().Create(ctx, makeEntry("cat-sl", "alice", "my-skill")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Delist.
	if err := st.CustomizationCatalog().SetListed(ctx, "cat-sl", false); err != nil {
		t.Fatalf("SetListed false: %v", err)
	}

	got, _ := st.CustomizationCatalog().Get(ctx, "cat-sl")
	if got.Listed {
		t.Error("expected listed=false after SetListed(false)")
	}

	// Check it's not in global List.
	all, _ := st.CustomizationCatalog().List(ctx)
	if len(all) != 0 {
		t.Errorf("expected 0 listed entries after delist, got %d", len(all))
	}

	// Relist.
	if err := st.CustomizationCatalog().SetListed(ctx, "cat-sl", true); err != nil {
		t.Fatalf("SetListed true: %v", err)
	}
	all, _ = st.CustomizationCatalog().List(ctx)
	if len(all) != 1 {
		t.Errorf("expected 1 listed entry after relist, got %d", len(all))
	}
}

func TestCustomizationCatalog_SetListed_NotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	err := st.CustomizationCatalog().SetListed(ctx, "no-such", false)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestCustomizationCatalog_Delete(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.CustomizationCatalog().Create(ctx, makeEntry("cat-del", "alice", "to-delete")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := st.CustomizationCatalog().Delete(ctx, "cat-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := st.CustomizationCatalog().Get(ctx, "cat-del")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound after Delete, got %v", err)
	}
}

func TestCustomizationCatalog_Delete_NotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	err := st.CustomizationCatalog().Delete(ctx, "no-such")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
