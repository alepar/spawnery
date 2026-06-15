package cp

import (
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
)

var skillContent = []byte("skill-content")

func createTestCatalogEntry(t *testing.T, s *Server, kind cpv1.ProfileEntryKind, name string) string {
	t.Helper()
	resp, err := s.CreateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.CreateCatalogEntryRequest{
		Kind:        kind,
		Name:        name,
		Description: "test description",
		Content:     skillContent,
	}))
	if err != nil {
		t.Fatalf("CreateCatalogEntry: %v", err)
	}
	return resp.Msg.CatalogId
}

// --- CreateCatalogEntry -------------------------------------------------------

func TestCreateCatalogEntry_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	resp, err := s.CreateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.CreateCatalogEntryRequest{
		Kind:        cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:        "my-skill",
		Description: "A test skill",
		Content:     skillContent,
	}))
	if err != nil {
		t.Fatalf("CreateCatalogEntry: %v", err)
	}
	if resp.Msg.CatalogId == "" {
		t.Error("expected non-empty catalog_id")
	}
}

func TestCreateCatalogEntry_Unauthenticated(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.CreateCatalogEntry(noAuthCtx(), connect.NewRequest(&cpv1.CreateCatalogEntryRequest{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "sk", Content: skillContent,
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("expected Unauthenticated, got %v", err)
	}
}

func TestCreateCatalogEntry_UnspecifiedKind(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.CreateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.CreateCatalogEntryRequest{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_UNSPECIFIED, Name: "sk", Content: skillContent,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for unspecified kind, got %v", err)
	}
}

func TestCreateCatalogEntry_InvalidName(t *testing.T) {
	s, _, _ := newTestServer(t)

	for _, name := range []string{"", "foo/bar", ".", ".."} {
		_, err := s.CreateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.CreateCatalogEntryRequest{
			Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: name, Content: skillContent,
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("name %q: expected InvalidArgument, got %v", name, err)
		}
	}
}

func TestCreateCatalogEntry_EmptyContent(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.CreateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.CreateCatalogEntryRequest{
		Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "sk", Content: nil,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for empty content, got %v", err)
	}
}

// --- GetCatalogEntry ---------------------------------------------------------

func TestGetCatalogEntry_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	catalogID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	resp, err := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catalogID}))
	if err != nil {
		t.Fatalf("GetCatalogEntry: %v", err)
	}
	if resp.Msg.Entry.CatalogId != catalogID {
		t.Errorf("catalog_id mismatch: got %q", resp.Msg.Entry.CatalogId)
	}
	if resp.Msg.Entry.Name != "my-skill" {
		t.Errorf("name mismatch: got %q", resp.Msg.Entry.Name)
	}
	if resp.Msg.Entry.CreatorId != "alice" {
		t.Errorf("creator_id mismatch: got %q", resp.Msg.Entry.CreatorId)
	}
	if !resp.Msg.Entry.Listed {
		t.Error("expected listed=true")
	}
}

func TestGetCatalogEntry_AnyOwnerCanRead(t *testing.T) {
	s, _, _ := newTestServer(t)

	catalogID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	// Bob (not creator) can read Alice's entry.
	_, err := s.GetCatalogEntry(bobCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catalogID}))
	if err != nil {
		t.Errorf("bob should be able to read any catalog entry, got: %v", err)
	}
}

func TestGetCatalogEntry_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: "no-such"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestGetCatalogEntry_Unauthenticated(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.GetCatalogEntry(noAuthCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: "any"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("expected Unauthenticated, got %v", err)
	}
}

// --- ListCatalogEntries -------------------------------------------------------

func TestListCatalogEntries_ListedOnly(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	// Delist it; should not appear in global list.
	if _, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID, Listed: false,
	})); err != nil {
		t.Fatalf("SetCatalogListing: %v", err)
	}

	resp, err := s.ListCatalogEntries(aliceCtx(), connect.NewRequest(&cpv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatalf("ListCatalogEntries: %v", err)
	}
	if len(resp.Msg.Entries) != 0 {
		t.Errorf("expected 0 entries after delist, got %d", len(resp.Msg.Entries))
	}
}

func TestListCatalogEntries_AnyOwnerCanList(t *testing.T) {
	s, _, _ := newTestServer(t)

	createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "alice-skill")

	// Bob can see Alice's listed entry.
	resp, err := s.ListCatalogEntries(bobCtx(), connect.NewRequest(&cpv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatalf("ListCatalogEntries bob: %v", err)
	}
	if len(resp.Msg.Entries) != 1 {
		t.Errorf("expected 1 entry visible to bob, got %d", len(resp.Msg.Entries))
	}
	// Content must NOT be in the summary.
	summary := resp.Msg.Entries[0]
	if summary.CatalogId == "" || summary.Name == "" {
		t.Errorf("unexpected empty fields in summary: %+v", summary)
	}
}

func TestListCatalogEntries_Unauthenticated(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.ListCatalogEntries(noAuthCtx(), connect.NewRequest(&cpv1.ListCatalogEntriesRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("expected Unauthenticated, got %v", err)
	}
}

// --- UpdateCatalogEntry -------------------------------------------------------

func TestUpdateCatalogEntry_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "original")

	_, err := s.UpdateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateCatalogEntryRequest{
		CatalogId:   catID,
		Name:        "renamed",
		Description: "new desc",
		Content:     []byte("new content"),
	}))
	if err != nil {
		t.Fatalf("UpdateCatalogEntry: %v", err)
	}

	gr, _ := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID}))
	if gr.Msg.Entry.Name != "renamed" {
		t.Errorf("expected 'renamed', got %q", gr.Msg.Entry.Name)
	}
	if gr.Msg.Entry.Description != "new desc" {
		t.Errorf("expected 'new desc', got %q", gr.Msg.Entry.Description)
	}
}

func TestUpdateCatalogEntry_NotCreator_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "alice-skill")

	_, err := s.UpdateCatalogEntry(bobCtx(), connect.NewRequest(&cpv1.UpdateCatalogEntryRequest{
		CatalogId: catID, Name: "hijacked", Content: []byte("bad"),
	}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied for non-creator, got %v", err)
	}
}

func TestUpdateCatalogEntry_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.UpdateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateCatalogEntryRequest{
		CatalogId: "no-such", Name: "x", Content: []byte("c"),
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestUpdateCatalogEntry_InvalidContent(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	_, err := s.UpdateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateCatalogEntryRequest{
		CatalogId: catID, Name: "my-skill", Content: nil, // empty content
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for empty content on update, got %v", err)
	}
}

// --- DeleteCatalogEntry -------------------------------------------------------

func TestDeleteCatalogEntry_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "to-delete")

	_, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{CatalogId: catID}))
	if err != nil {
		t.Fatalf("DeleteCatalogEntry: %v", err)
	}

	_, err = s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound after delete, got %v", err)
	}
}

func TestDeleteCatalogEntry_NotCreator_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "alice-skill")

	_, err := s.DeleteCatalogEntry(bobCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{CatalogId: catID}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied for non-creator delete, got %v", err)
	}
}

func TestDeleteCatalogEntry_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.DeleteCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{CatalogId: "no-such"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

// --- SetCatalogListing -------------------------------------------------------

func TestSetCatalogListing_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	// Delist.
	_, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID, Listed: false,
	}))
	if err != nil {
		t.Fatalf("SetCatalogListing false: %v", err)
	}

	gr, _ := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID}))
	if gr.Msg.Entry.Listed {
		t.Error("expected listed=false after SetCatalogListing(false)")
	}

	// Relist.
	_, err = s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID, Listed: true,
	}))
	if err != nil {
		t.Fatalf("SetCatalogListing true: %v", err)
	}

	gr, _ = s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID}))
	if !gr.Msg.Entry.Listed {
		t.Error("expected listed=true after SetCatalogListing(true)")
	}
}

func TestSetCatalogListing_NotCreator_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "alice-skill")

	_, err := s.SetCatalogListing(bobCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID, Listed: false,
	}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied for non-creator, got %v", err)
	}
}

func TestSetCatalogListing_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: "no-such", Listed: false,
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}
