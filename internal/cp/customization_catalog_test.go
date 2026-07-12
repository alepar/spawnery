package cp

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/skillfetch"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
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

// TestCreateCatalogEntry_UnlistedByDefault verifies the sp-mwco.3.4 §4.6 D1 rule: every newly
// created row (inline entries too, not just URL-ingested ones) starts unlisted.
func TestCreateCatalogEntry_UnlistedByDefault(t *testing.T) {
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

	gr, err := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: resp.Msg.CatalogId}))
	if err != nil {
		t.Fatalf("GetCatalogEntry: %v", err)
	}
	if gr.Msg.Entry.Listed {
		t.Error("expected listed=false for a freshly created entry")
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
	// Unlisted by default (sp-mwco.3.4 §4.6 D1).
	if resp.Msg.Entry.Listed {
		t.Error("expected listed=false")
	}
}

// TestGetCatalogEntry_UnlistedForeignOwner_NotFound verifies the tenant gate (§4.6 D6): a
// freshly created (unlisted) entry is invisible to anyone but its creator.
func TestGetCatalogEntry_UnlistedForeignOwner_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)

	catalogID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	// Bob (not creator) cannot see Alice's unlisted entry — NotFound, not PermissionDenied (don't
	// confirm existence).
	_, err := s.GetCatalogEntry(bobCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catalogID}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound for a foreign unlisted entry, got %v", err)
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

// TestListCatalogEntries_UnlistedInvisibleToOtherTenant verifies the tenant-scoped visibility
// rule (sp-mwco.3.4 §4.6 D2): "listed OR mine". A freshly created (unlisted) entry is visible to
// its creator but invisible to every other tenant.
func TestListCatalogEntries_UnlistedInvisibleToOtherTenant(t *testing.T) {
	s, _, _ := newTestServer(t)

	createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	aliceResp, err := s.ListCatalogEntries(aliceCtx(), connect.NewRequest(&cpv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatalf("ListCatalogEntries alice: %v", err)
	}
	if len(aliceResp.Msg.Entries) != 1 {
		t.Errorf("alice should see her own unlisted entry, got %d", len(aliceResp.Msg.Entries))
	}

	bobResp, err := s.ListCatalogEntries(bobCtx(), connect.NewRequest(&cpv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatalf("ListCatalogEntries bob: %v", err)
	}
	if len(bobResp.Msg.Entries) != 0 {
		t.Errorf("bob should NOT see alice's unlisted entry, got %d", len(bobResp.Msg.Entries))
	}
}

// TestListCatalogEntries_PublishedVisibleToAllTenants verifies that once an admin publishes an
// entry, it becomes visible to every tenant (not just the creator).
func TestListCatalogEntries_PublishedVisibleToAllTenants(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "alice-skill")
	if _, err := s.PublishCatalogEntry(adminCtx(), connect.NewRequest(&cpv1.PublishCatalogEntryRequest{CatalogId: catID})); err != nil {
		t.Fatalf("PublishCatalogEntry: %v", err)
	}

	resp, err := s.ListCatalogEntries(bobCtx(), connect.NewRequest(&cpv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatalf("ListCatalogEntries bob: %v", err)
	}
	if len(resp.Msg.Entries) != 1 {
		t.Errorf("expected 1 published entry visible to bob, got %d", len(resp.Msg.Entries))
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

func TestUpdateCatalogEntry_URLIngestedRowRejected(t *testing.T) {
	s, _, _ := newTestServer(t)
	result := makeCannedResult("url-skill")
	fakeStore := skillstore.NewFakeSkillStore()
	s.SetSkillIngest(&fakeFetcher{result: result}, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	ingestResp, err := s.IngestSkillFromURL(aliceCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "testowner/testrepo",
	}))
	if err != nil {
		t.Fatalf("IngestSkillFromURL: %v", err)
	}
	catID := ingestResp.Msg.CatalogId

	_, err = s.UpdateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateCatalogEntryRequest{
		CatalogId:   catID,
		Name:        "renamed",
		Description: "new desc",
		Content:     []byte("new content"),
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition for URL-ingested row, got %v", err)
	}

	gr, gerr := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID}))
	if gerr != nil {
		t.Fatalf("GetCatalogEntry: %v", gerr)
	}
	if gr.Msg.Entry.Name != "url-skill" {
		t.Errorf("name changed despite rejected update: got %q", gr.Msg.Entry.Name)
	}
	// The bundle ingest path (sp-mwco.1.4) seeds Description from the discovered member, so the
	// baseline here is the ingest-time description -- what matters is that the rejected update did
	// not overwrite it with "new desc".
	if gr.Msg.Entry.Description != "test description" {
		t.Errorf("description changed despite rejected update: got %q", gr.Msg.Entry.Description)
	}
	if len(gr.Msg.Entry.Content) != 0 {
		t.Errorf("content changed despite rejected update: got %q", gr.Msg.Entry.Content)
	}
}

func TestUpdateCatalogEntry_InlineRowStillUpdatable(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "inline-skill")

	_, err := s.UpdateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.UpdateCatalogEntryRequest{
		CatalogId:   catID,
		Name:        "inline-renamed",
		Description: "inline new desc",
		Content:     []byte("inline new content"),
	}))
	if err != nil {
		t.Fatalf("UpdateCatalogEntry on inline row: %v", err)
	}

	gr, gerr := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID}))
	if gerr != nil {
		t.Fatalf("GetCatalogEntry: %v", gerr)
	}
	if gr.Msg.Entry.Name != "inline-renamed" {
		t.Errorf("expected inline row to update, got name %q", gr.Msg.Entry.Name)
	}
}

func TestUpdateCatalogEntry_URLIngestedRow_NonCreator_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)
	result := makeCannedResult("url-skill-2")
	fakeStore := skillstore.NewFakeSkillStore()
	s.SetSkillIngest(&fakeFetcher{result: result}, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	ingestResp, err := s.IngestSkillFromURL(aliceCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "testowner/testrepo",
	}))
	if err != nil {
		t.Fatalf("IngestSkillFromURL: %v", err)
	}
	catID := ingestResp.Msg.CatalogId

	_, err = s.UpdateCatalogEntry(bobCtx(), connect.NewRequest(&cpv1.UpdateCatalogEntryRequest{
		CatalogId: catID, Name: "hijacked", Content: []byte("bad"),
	}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied (ownership checked before immutability), got %v", err)
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

// TestSetCatalogListing_CreatorCanUnlist verifies the creator may unlist (idempotent — the entry
// is already unlisted by default, D1) without needing to be an admin.
func TestSetCatalogListing_CreatorCanUnlist(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

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
}

// TestSetCatalogListing_Relist_RequiresAdmin verifies §4.6 D4: listed=true via the legacy verb
// is admin-only — otherwise it is a trivial bypass of PublishCatalogEntry's admin gate.
func TestSetCatalogListing_Relist_RequiresAdmin(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "my-skill")

	// Non-admin creator cannot relist via the legacy verb.
	if _, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID, Listed: true,
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied for non-admin relist, got %v", err)
	}

	// Admin can.
	if _, err := s.SetCatalogListing(adminCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID, Listed: true,
	})); err != nil {
		t.Fatalf("SetCatalogListing true (admin): %v", err)
	}

	gr, err := s.GetCatalogEntry(bobCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catID}))
	if err != nil {
		t.Fatalf("bob GetCatalogEntry after admin relist: %v", err)
	}
	if !gr.Msg.Entry.Listed {
		t.Error("expected listed=true after admin relist")
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

// --- Guarded unlisting (sp-mwco.3.4 §4.6 D5) -----------------------------------

func TestSetCatalogListing_GuardedUnlist_NoReferences_Succeeds(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "unused-skill")

	resp, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID, Listed: false, Confirm: false,
	}))
	if err != nil {
		t.Fatalf("SetCatalogListing: %v", err)
	}
	if resp.Msg.ReferencedProfiles != 0 || resp.Msg.ReferencedOwners != 0 ||
		resp.Msg.ReferencedBundleVersions != 0 || resp.Msg.TerminatedSpawns != 0 {
		t.Errorf("expected all-zero counts for an unreferenced entry, got %+v", resp.Msg)
	}
}

func TestSetCatalogListing_GuardedUnlist_References_RequiresConfirm(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "widely-used")

	// 3 profiles across 2 owners.
	pf1 := uuid.NewString()
	createProfileForKS(t, s, pf1, "bob")
	addCatalogRefEntryForKS(t, s, pf1, "e1", catID)

	pf2 := uuid.NewString()
	createProfileForKS(t, s, pf2, "bob")
	addCatalogRefEntryForKS(t, s, pf2, "e2", catID)

	pf3 := uuid.NewString()
	createProfileForKS(t, s, pf3, "carol")
	addCatalogRefEntryForKS(t, s, pf3, "e3", catID)

	// A running spawn so the "would terminate N" count is exercised too.
	makeSpawnForKS(t, s, "sp-guard-1", "bob", pf1)

	// 1 bundle-version membership.
	if err := s.st.SkillBundles().Create(context.Background(), store.SkillBundle{
		BundleID: "bun-guard-1", CreatorID: "alice", Name: "b", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("SkillBundles().Create: %v", err)
	}
	if err := s.st.SkillBundles().CreateVersion(context.Background(),
		store.SkillBundleVersion{VersionID: "ver-guard-1", BundleID: "bun-guard-1", Seq: 1, CreatedAt: 1},
		[]store.SkillBundleMember{{SourceSubdir: "skill-a", CatalogID: catID, Position: 0}},
	); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	_, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID, Listed: false, Confirm: false,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "3 profile") || !strings.Contains(msg, "2 owner") || !strings.Contains(msg, "1 bundle version") {
		t.Errorf("expected message to contain reference counts, got %q", msg)
	}
	// Disclosure regression (same rule as the delete path, sp-mwco.3.3 §4.3): never leak ids.
	for _, id := range []string{pf1, pf2, pf3, "bob", "carol", "sp-guard-1"} {
		if strings.Contains(msg, id) {
			t.Errorf("error message leaks id %q: %q", id, msg)
		}
	}

	// Nothing terminated before confirm=true.
	if isDeleted(t, s, "sp-guard-1") {
		t.Error("spawn must NOT be terminated before confirm=true")
	}
}

func TestSetCatalogListing_GuardedUnlist_ConfirmTrue_Succeeds(t *testing.T) {
	s, _, _ := newTestServer(t)

	catID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "widely-used")

	pfID := uuid.NewString()
	createProfileForKS(t, s, pfID, "bob")
	addCatalogRefEntryForKS(t, s, pfID, "e1", catID)
	makeSpawnForKS(t, s, "sp-guard-2", "bob", pfID)

	resp, err := s.SetCatalogListing(aliceCtx(), connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catID, Listed: false, Confirm: true,
	}))
	if err != nil {
		t.Fatalf("SetCatalogListing confirm=true: %v", err)
	}
	if resp.Msg.ReferencedProfiles != 1 || resp.Msg.ReferencedOwners != 1 || resp.Msg.TerminatedSpawns != 1 {
		t.Errorf("unexpected counts: %+v", resp.Msg)
	}
	if !isDeleted(t, s, "sp-guard-2") {
		t.Error("expected spawn to be terminated after confirmed unlist")
	}
}
