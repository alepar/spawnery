package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/store"
)

// ptrString and ptrInt64 are local test helpers — the store's SHA256/Size columns are nullable
// (nil = no content identity for inline entries).
func ptrString(s string) *string { return &s }
func ptrInt64(i int64) *int64    { return &i }

const (
	testSourceCommit = "1111111111111111111111111111111111aaaa"
	testSHA256       = "2222222222222222222222222222222222222222222222222222222222bbbb"
)

func seedURLIngestedEntry(t *testing.T, st store.Store, catalogID, creatorID string) store.CustomizationCatalogEntry {
	t.Helper()
	e := store.CustomizationCatalogEntry{
		CatalogID:    catalogID,
		CreatorID:    creatorID,
		Kind:         string(store.ProfileEntrySkill),
		Name:         "provenance-skill",
		Description:  "a url-ingested skill",
		Content:      skillContent,
		Listed:       true,
		CreatedAt:    1000,
		UpdatedAt:    1000,
		SourceURL:    "obra/superpowers",
		SourceRef:    "main",
		SourceSubdir: "skills/x",
		SHA256:       ptrString(testSHA256),
		Size:         ptrInt64(4096),
		BundleMember: true,
		SourceCommit: testSourceCommit,
	}
	if err := st.CustomizationCatalog().Create(context.Background(), e); err != nil {
		t.Fatalf("seed CustomizationCatalog().Create: %v", err)
	}
	return e
}

func assertProvenanceFields(t *testing.T, label string, sourceURL, sourceRef, sourceSubdir, sourceCommit, sha256 string, size int64, bundleMember bool) {
	t.Helper()
	if sourceURL != "https://github.com/obra/superpowers" {
		t.Errorf("%s: SourceUrl = %q, want normalized github URL", label, sourceURL)
	}
	if sourceRef != "main" {
		t.Errorf("%s: SourceRef = %q, want %q", label, sourceRef, "main")
	}
	if sourceSubdir != "skills/x" {
		t.Errorf("%s: SourceSubdir = %q, want %q", label, sourceSubdir, "skills/x")
	}
	if sourceCommit != testSourceCommit {
		t.Errorf("%s: SourceCommit = %q, want %q", label, sourceCommit, testSourceCommit)
	}
	if sha256 != testSHA256 {
		t.Errorf("%s: Sha256 = %q, want %q", label, sha256, testSHA256)
	}
	if size != 4096 {
		t.Errorf("%s: Size = %d, want 4096", label, size)
	}
	if !bundleMember {
		t.Errorf("%s: BundleMember = false, want true", label)
	}
}

// TestGetCatalogEntryReturnsProvenance verifies that GetCatalogEntry carries all seven
// provenance fields for a URL-ingested skill, and that source_url is rendered as a resolvable
// URL (D1) even though the store persists the bare "owner/repo" slug.
func TestGetCatalogEntryReturnsProvenance(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedURLIngestedEntry(t, s.st, "cat-prov-1", "alice")

	resp, err := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: "cat-prov-1"}))
	if err != nil {
		t.Fatalf("GetCatalogEntry: %v", err)
	}
	e := resp.Msg.Entry
	assertProvenanceFields(t, "GetCatalogEntry", e.SourceUrl, e.SourceRef, e.SourceSubdir, e.SourceCommit, e.Sha256, e.Size, e.BundleMember)
}

// TestListCatalogEntriesReturnsProvenance verifies that the CatalogEntrySummary returned by
// ListCatalogEntries carries the IDENTICAL seven provenance values as the full entry — the
// round-trip-through-both-response-types acceptance criterion.
func TestListCatalogEntriesReturnsProvenance(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedURLIngestedEntry(t, s.st, "cat-prov-2", "alice")

	resp, err := s.ListCatalogEntries(aliceCtx(), connect.NewRequest(&cpv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatalf("ListCatalogEntries: %v", err)
	}
	var got *cpv1.CatalogEntrySummary
	for _, e := range resp.Msg.Entries {
		if e.CatalogId == "cat-prov-2" {
			got = e
		}
	}
	if got == nil {
		t.Fatalf("cat-prov-2 not found in ListCatalogEntries response")
	}
	assertProvenanceFields(t, "ListCatalogEntries", got.SourceUrl, got.SourceRef, got.SourceSubdir, got.SourceCommit, got.Sha256, got.Size, got.BundleMember)
}

// TestCatalogProvenanceEmptyForInlineEntry verifies that an inline (non-URL) entry — created via
// CreateCatalogEntry, where SourceURL/Ref/Subdir are "" and SHA256/Size are nil — round-trips
// with every provenance field zero-valued on BOTH response types, and that the nil SHA256/Size
// pointers do not panic.
func TestCatalogProvenanceEmptyForInlineEntry(t *testing.T) {
	s, _, _ := newTestServer(t)
	catalogID := createTestCatalogEntry(t, s, cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, "inline-skill")

	getResp, err := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catalogID}))
	if err != nil {
		t.Fatalf("GetCatalogEntry: %v", err)
	}
	e := getResp.Msg.Entry
	if e.SourceUrl != "" || e.SourceRef != "" || e.SourceSubdir != "" || e.SourceCommit != "" || e.Sha256 != "" || e.Size != 0 || e.BundleMember {
		t.Errorf("GetCatalogEntry: expected zero-valued provenance for inline entry, got %+v", e)
	}

	// Listed defaults to false (sp-mwco.3.4 D1), so list it first to make it visible via
	// ListVisibleTo — alice is still the creator either way, but keep the assertion honest.
	listResp, err := s.ListCatalogEntries(aliceCtx(), connect.NewRequest(&cpv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatalf("ListCatalogEntries: %v", err)
	}
	var got *cpv1.CatalogEntrySummary
	for _, e := range listResp.Msg.Entries {
		if e.CatalogId == catalogID {
			got = e
		}
	}
	if got == nil {
		t.Fatalf("%s not found in ListCatalogEntries response", catalogID)
	}
	if got.SourceUrl != "" || got.SourceRef != "" || got.SourceSubdir != "" || got.SourceCommit != "" || got.Sha256 != "" || got.Size != 0 || got.BundleMember {
		t.Errorf("ListCatalogEntries: expected zero-valued provenance for inline entry, got %+v", got)
	}
}

// TestProvenanceSourceURLPassthrough is a table test on provenanceSourceURL (D1) plus a
// through-Get check: a value that already has a scheme is emitted verbatim (not
// double-prefixed); a bare slug gets the github.com prefix; "" stays "".
func TestProvenanceSourceURLPassthrough(t *testing.T) {
	tests := []struct {
		name   string
		stored string
		want   string
	}{
		{"bare slug", "obra/superpowers", "https://github.com/obra/superpowers"},
		{"already https", "https://github.com/obra/superpowers", "https://github.com/obra/superpowers"},
		{"already http", "http://example.com/obra/superpowers", "http://example.com/obra/superpowers"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := provenanceSourceURL(tt.stored); got != tt.want {
				t.Errorf("provenanceSourceURL(%q) = %q, want %q", tt.stored, got, tt.want)
			}
		})
	}

	// Through-Get: a row with a stored URL that already has a scheme is emitted verbatim.
	s, _, _ := newTestServer(t)
	e := seedURLIngestedEntry(t, s.st, "cat-prov-3", "alice")
	e.CatalogID = "cat-prov-4"
	e.SourceURL = "https://gitlab.example.com/obra/superpowers"
	// Distinct SHA256 — the unique index is (creator_id, sha256); reusing cat-prov-3's would collide.
	e.SHA256 = ptrString("3333333333333333333333333333333333333333333333333333333333cccc")
	if err := s.st.CustomizationCatalog().Create(context.Background(), e); err != nil {
		t.Fatalf("seed: %v", err)
	}
	resp, err := s.GetCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: "cat-prov-4"}))
	if err != nil {
		t.Fatalf("GetCatalogEntry: %v", err)
	}
	if resp.Msg.Entry.SourceUrl != "https://gitlab.example.com/obra/superpowers" {
		t.Errorf("SourceUrl = %q, want verbatim passthrough", resp.Msg.Entry.SourceUrl)
	}
}
