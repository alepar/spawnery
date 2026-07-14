package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
)

// fakeCatalogClient is a canned catalogClient that records each request.
type fakeCatalogClient struct {
	createResp  *cpv1.CreateCatalogEntryResponse
	getResp     *cpv1.GetCatalogEntryResponse
	listResp    *cpv1.ListCatalogEntriesResponse
	ingestResp  *cpv1.IngestSkillFromURLResponse
	ingestErr   error
	denyErr     error
	allowErr    error
	denialsResp *cpv1.ListSkillObjectDenialsResponse

	gotCreate  *cpv1.CreateCatalogEntryRequest
	gotUpdate  *cpv1.UpdateCatalogEntryRequest
	gotDelete  *cpv1.DeleteCatalogEntryRequest
	gotListing *cpv1.SetCatalogListingRequest
	gotIngest  *cpv1.IngestSkillFromURLRequest
	gotDeny    *cpv1.DenySkillObjectRequest
	gotAllow   *cpv1.AllowSkillObjectRequest
}

func (f *fakeCatalogClient) CreateCatalogEntry(_ context.Context, r *connect.Request[cpv1.CreateCatalogEntryRequest]) (*connect.Response[cpv1.CreateCatalogEntryResponse], error) {
	f.gotCreate = r.Msg
	return connect.NewResponse(f.createResp), nil
}

func (f *fakeCatalogClient) GetCatalogEntry(_ context.Context, _ *connect.Request[cpv1.GetCatalogEntryRequest]) (*connect.Response[cpv1.GetCatalogEntryResponse], error) {
	return connect.NewResponse(f.getResp), nil
}

func (f *fakeCatalogClient) ListCatalogEntries(_ context.Context, _ *connect.Request[cpv1.ListCatalogEntriesRequest]) (*connect.Response[cpv1.ListCatalogEntriesResponse], error) {
	return connect.NewResponse(f.listResp), nil
}

func (f *fakeCatalogClient) UpdateCatalogEntry(_ context.Context, r *connect.Request[cpv1.UpdateCatalogEntryRequest]) (*connect.Response[cpv1.UpdateCatalogEntryResponse], error) {
	f.gotUpdate = r.Msg
	return connect.NewResponse(&cpv1.UpdateCatalogEntryResponse{}), nil
}

func (f *fakeCatalogClient) DeleteCatalogEntry(_ context.Context, r *connect.Request[cpv1.DeleteCatalogEntryRequest]) (*connect.Response[cpv1.DeleteCatalogEntryResponse], error) {
	f.gotDelete = r.Msg
	return connect.NewResponse(&cpv1.DeleteCatalogEntryResponse{}), nil
}

func (f *fakeCatalogClient) SetCatalogListing(_ context.Context, r *connect.Request[cpv1.SetCatalogListingRequest]) (*connect.Response[cpv1.SetCatalogListingResponse], error) {
	f.gotListing = r.Msg
	return connect.NewResponse(&cpv1.SetCatalogListingResponse{}), nil
}

func (f *fakeCatalogClient) IngestSkillFromURL(_ context.Context, r *connect.Request[cpv1.IngestSkillFromURLRequest]) (*connect.Response[cpv1.IngestSkillFromURLResponse], error) {
	f.gotIngest = r.Msg
	if f.ingestErr != nil {
		return nil, f.ingestErr
	}
	return connect.NewResponse(f.ingestResp), nil
}

func (f *fakeCatalogClient) DenySkillObject(_ context.Context, r *connect.Request[cpv1.DenySkillObjectRequest]) (*connect.Response[cpv1.DenySkillObjectResponse], error) {
	f.gotDeny = r.Msg
	if f.denyErr != nil {
		return nil, f.denyErr
	}
	return connect.NewResponse(&cpv1.DenySkillObjectResponse{}), nil
}

func (f *fakeCatalogClient) AllowSkillObject(_ context.Context, r *connect.Request[cpv1.AllowSkillObjectRequest]) (*connect.Response[cpv1.AllowSkillObjectResponse], error) {
	f.gotAllow = r.Msg
	if f.allowErr != nil {
		return nil, f.allowErr
	}
	return connect.NewResponse(&cpv1.AllowSkillObjectResponse{}), nil
}

func (f *fakeCatalogClient) ListSkillObjectDenials(_ context.Context, _ *connect.Request[cpv1.ListSkillObjectDenialsRequest]) (*connect.Response[cpv1.ListSkillObjectDenialsResponse], error) {
	return connect.NewResponse(f.denialsResp), nil
}

// ---- create ----

func TestRunCatalogCreate(t *testing.T) {
	f := &fakeCatalogClient{createResp: &cpv1.CreateCatalogEntryResponse{CatalogId: "cat-1"}}
	var out bytes.Buffer
	p := catalogCreateParams{Kind: "mcp", Name: "n", Description: "d", Content: []byte("c")}
	if err := runCatalogCreate(context.Background(), f, &out, p); err != nil {
		t.Fatal(err)
	}
	if f.gotCreate.GetKind() != cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP || f.gotCreate.GetName() != "n" {
		t.Fatalf("create req = %+v", f.gotCreate)
	}
	if !strings.Contains(out.String(), "cat-1") {
		t.Fatalf("output missing id: %q", out.String())
	}
	if !strings.Contains(out.String(), "unlisted") || !strings.Contains(out.String(), "admin") {
		t.Fatalf("output missing unlisted-default/admin-publish hint: %q", out.String())
	}
}

func TestCatalogCreateHelpMentionsUnlistedDefault(t *testing.T) {
	if !strings.Contains(catalogCreateCmd().Usage, "unlisted") {
		t.Fatalf("catalog create Usage missing unlisted default: %q", catalogCreateCmd().Usage)
	}
}

// ---- list ----

func TestRunCatalogList(t *testing.T) {
	f := &fakeCatalogClient{listResp: &cpv1.ListCatalogEntriesResponse{Entries: []*cpv1.CatalogEntrySummary{
		{CatalogId: "cat-1", Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, Name: "s", Description: "d"},
	}}}
	var out bytes.Buffer
	if err := runCatalogList(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cat-1") || !strings.Contains(out.String(), "skill") {
		t.Fatalf("list output = %q", out.String())
	}
}

// ---- show ----

func TestRunCatalogShow(t *testing.T) {
	f := &fakeCatalogClient{getResp: &cpv1.GetCatalogEntryResponse{
		Entry: &cpv1.CustomizationCatalogEntry{
			CatalogId:   "cat-42",
			Kind:        cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG,
			Name:        "cfg-tool",
			Description: "a config",
			Content:     []byte("some content"),
			Listed:      true,
		},
	}}
	var out bytes.Buffer
	if err := runCatalogShow(context.Background(), f, &out, "cat-42"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "cat-42") || !strings.Contains(s, "cfg-tool") || !strings.Contains(s, "some content") {
		t.Fatalf("show output = %q", s)
	}
}

// TestRunCatalogShowProvenance verifies catalog show prints the seven provenance fields for a
// URL-ingested entry, with the content hash and the upstream commit under distinct labels
// (sp-mwco.3.1 D2 — the anti-mislabel regression test), and prints none of them for an inline
// entry.
func TestRunCatalogShowProvenance(t *testing.T) {
	f := &fakeCatalogClient{getResp: &cpv1.GetCatalogEntryResponse{
		Entry: &cpv1.CustomizationCatalogEntry{
			CatalogId:    "cat-42",
			Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
			Name:         "prov-skill",
			Description:  "a url-ingested skill",
			Listed:       true,
			SourceUrl:    "https://github.com/obra/superpowers",
			SourceRef:    "main",
			SourceSubdir: "skills/x",
			SourceCommit: "1111111111111111111111111111111111aaaa",
			Sha256:       "2222222222222222222222222222222222222222222222222222222222bbbb",
			Size:         4096,
			BundleMember: true,
		},
	}}
	var out bytes.Buffer
	if err := runCatalogShow(context.Background(), f, &out, "cat-42"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"Source:", "https://github.com/obra/superpowers",
		"Source ref:", "main",
		"Source subdir:", "skills/x",
		"Source commit:", "1111111111111111111111111111111111aaaa",
		"Content SHA-256:", "2222222222222222222222222222222222222222222222222222222222bbbb",
		"Size:", "4096",
		"Bundle member: true",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("show output missing %q; got:\n%s", want, s)
		}
	}
}

func TestRunCatalogShowProvenance_InlineEntryOmitsIt(t *testing.T) {
	f := &fakeCatalogClient{getResp: &cpv1.GetCatalogEntryResponse{
		Entry: &cpv1.CustomizationCatalogEntry{
			CatalogId:   "cat-inline",
			Kind:        cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG,
			Name:        "cfg-tool",
			Description: "a config",
			Content:     []byte("some content"),
			Listed:      true,
		},
	}}
	var out bytes.Buffer
	if err := runCatalogShow(context.Background(), f, &out, "cat-inline"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Contains(s, "Content SHA-256") {
		t.Errorf("show output should not contain Content SHA-256 for inline entry: %q", s)
	}
	if strings.Contains(s, "Source:") {
		t.Errorf("show output should not contain Source: for inline entry: %q", s)
	}
}

// TestRunCatalogListProvenance verifies catalog list gains SOURCE and COMMIT columns, with the
// commit shortened to 12 chars, and inline rows leave both blank.
func TestRunCatalogListProvenance(t *testing.T) {
	f := &fakeCatalogClient{listResp: &cpv1.ListCatalogEntriesResponse{Entries: []*cpv1.CatalogEntrySummary{
		{
			CatalogId:    "cat-1",
			Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
			Name:         "s",
			Description:  "d",
			SourceUrl:    "https://github.com/obra/superpowers",
			SourceCommit: "1111111111111111111111111111111111aaaa",
		},
		{
			CatalogId:   "cat-2",
			Kind:        cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG,
			Name:        "inline",
			Description: "d2",
		},
	}}}
	var out bytes.Buffer
	if err := runCatalogList(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "SOURCE") || !strings.Contains(s, "COMMIT") {
		t.Fatalf("list header missing SOURCE/COMMIT columns: %q", s)
	}
	if !strings.Contains(s, "cat-1") || !strings.Contains(s, "skill") {
		t.Fatalf("list output = %q", s)
	}
	if !strings.Contains(s, "https://github.com/obra/superpowers") {
		t.Fatalf("list output missing source url: %q", s)
	}
	if strings.Contains(s, "1111111111111111111111111111111111aaaa") {
		t.Fatalf("list output should show the commit shortened to 12 chars, not the full 40: %q", s)
	}
	if !strings.Contains(s, "111111111111") {
		t.Fatalf("list output missing shortened (12-char) commit: %q", s)
	}
}

// ---- update ----

func TestRunCatalogUpdate(t *testing.T) {
	f := &fakeCatalogClient{}
	var out bytes.Buffer
	p := catalogUpdateParams{Name: "new-name", Description: "new-desc", Content: []byte("new content")}
	if err := runCatalogUpdate(context.Background(), f, &out, "cat-7", p); err != nil {
		t.Fatal(err)
	}
	if f.gotUpdate.GetCatalogId() != "cat-7" || f.gotUpdate.GetName() != "new-name" {
		t.Fatalf("update req = %+v", f.gotUpdate)
	}
}

// ---- delete ----

func TestRunCatalogDelete(t *testing.T) {
	f := &fakeCatalogClient{}
	var out bytes.Buffer
	if err := runCatalogDelete(context.Background(), f, &out, "cat-del"); err != nil {
		t.Fatal(err)
	}
	if f.gotDelete.GetCatalogId() != "cat-del" {
		t.Fatalf("delete req catalog id = %q", f.gotDelete.GetCatalogId())
	}
}

// ---- set-listing ----

func TestRunCatalogSetListing(t *testing.T) {
	f := &fakeCatalogClient{}
	var out bytes.Buffer
	if err := runCatalogSetListing(context.Background(), f, &out, "cat-1", true); err != nil {
		t.Fatal(err)
	}
	if f.gotListing.GetCatalogId() != "cat-1" || !f.gotListing.GetListed() {
		t.Fatalf("listing req = %+v", f.gotListing)
	}
}

// ---- ingest ----

func TestRunCatalogIngest(t *testing.T) {
	f := &fakeCatalogClient{ingestResp: &cpv1.IngestSkillFromURLResponse{CatalogId: "cat-9"}}
	var out bytes.Buffer
	p := catalogIngestParams{
		URL:         "owner/repo",
		Ref:         "v1.2.3",
		Subdir:      "skills/foo",
		Name:        "my-skill",
		Description: "a skill",
	}
	if err := runCatalogIngest(context.Background(), f, &out, p); err != nil {
		t.Fatal(err)
	}
	if f.gotIngest.GetUrl() != "owner/repo" || f.gotIngest.GetRef() != "v1.2.3" ||
		f.gotIngest.GetSubdir() != "skills/foo" || f.gotIngest.GetName() != "my-skill" ||
		f.gotIngest.GetDescription() != "a skill" {
		t.Fatalf("ingest req = %+v", f.gotIngest)
	}
	if !strings.Contains(out.String(), "cat-9") {
		t.Fatalf("output missing id: %q", out.String())
	}
	if !strings.Contains(out.String(), "unlisted") || !strings.Contains(out.String(), "admin") {
		t.Fatalf("output missing unlisted-default/admin-publish hint: %q", out.String())
	}
}

func TestRunCatalogIngest_ErrorPropagates(t *testing.T) {
	f := &fakeCatalogClient{ingestErr: errors.New("boom")}
	var out bytes.Buffer
	p := catalogIngestParams{URL: "owner/repo"}
	err := runCatalogIngest(context.Background(), f, &out, p)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}

func TestRunCatalogIngest_Bundle(t *testing.T) {
	f := &fakeCatalogClient{ingestResp: &cpv1.IngestSkillFromURLResponse{
		CatalogId:        "cat-9",
		BundleId:         "bun-1",
		VersionId:        "ver-1",
		MemberCatalogIds: []string{"cat-9", "cat-10", "cat-11"},
		Warnings:         []string{"nested skill skipped at skills/d/nested"},
		SkippedEntries:   []string{"skills/e/broken-symlink"},
		Changed:          true,
	}}
	var out bytes.Buffer
	p := catalogIngestParams{URL: "owner/repo"}
	if err := runCatalogIngest(context.Background(), f, &out, p); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"cat-9", "bun-1", "3",
		"nested skill skipped at skills/d/nested",
		"skills/e/broken-symlink",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ingest output missing %q; got:\n%s", want, s)
		}
	}
}

func TestRunCatalogIngest_Unchanged(t *testing.T) {
	f := &fakeCatalogClient{ingestResp: &cpv1.IngestSkillFromURLResponse{
		CatalogId: "cat-9",
		Changed:   false,
	}}
	var out bytes.Buffer
	p := catalogIngestParams{URL: "owner/repo"}
	if err := runCatalogIngest(context.Background(), f, &out, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already ingested; no new version") {
		t.Fatalf("expected unchanged note, got %q", out.String())
	}
}

// ---- deny / allow / denials ----

func TestRunCatalogDeny(t *testing.T) {
	f := &fakeCatalogClient{}
	var out bytes.Buffer
	sha := strings.Repeat("a", 64)
	if err := runCatalogDeny(context.Background(), f, &out, sha, "known-malicious"); err != nil {
		t.Fatal(err)
	}
	if f.gotDeny.GetSha256() != sha || f.gotDeny.GetReason() != "known-malicious" {
		t.Fatalf("deny req = %+v", f.gotDeny)
	}
	if !strings.Contains(out.String(), sha) {
		t.Fatalf("output missing sha: %q", out.String())
	}
}

func TestRunCatalogDeny_RequiresReason(t *testing.T) {
	f := &fakeCatalogClient{}
	var out bytes.Buffer
	err := runCatalogDeny(context.Background(), f, &out, strings.Repeat("a", 64), "  ")
	if err == nil {
		t.Fatal("expected error for empty reason")
	}
	if f.gotDeny != nil {
		t.Fatalf("expected no RPC to be issued, got %+v", f.gotDeny)
	}
}

func TestRunCatalogDeny_ErrorPropagates(t *testing.T) {
	f := &fakeCatalogClient{denyErr: connect.NewError(connect.CodePermissionDenied, errors.New("not an admin"))}
	var out bytes.Buffer
	err := runCatalogDeny(context.Background(), f, &out, strings.Repeat("a", 64), "reason")
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}

func TestRunCatalogAllow(t *testing.T) {
	f := &fakeCatalogClient{}
	var out bytes.Buffer
	sha := strings.Repeat("b", 64)
	if err := runCatalogAllow(context.Background(), f, &out, sha); err != nil {
		t.Fatal(err)
	}
	if f.gotAllow.GetSha256() != sha {
		t.Fatalf("allow req = %+v", f.gotAllow)
	}
	if !strings.Contains(out.String(), sha) {
		t.Fatalf("output missing sha: %q", out.String())
	}
}

func TestRunCatalogDenials(t *testing.T) {
	sha1 := strings.Repeat("c", 64)
	sha2 := strings.Repeat("d", 64)
	f := &fakeCatalogClient{denialsResp: &cpv1.ListSkillObjectDenialsResponse{
		Denials: []*cpv1.SkillObjectDenial{
			{Sha256: sha1, Reason: "malware", DeniedBy: "admin@example.com", CreatedAt: 1700000000},
			{Sha256: sha2, Reason: "license", DeniedBy: "admin2@example.com", CreatedAt: 1600000000},
		},
	}}
	var out bytes.Buffer
	if err := runCatalogDenials(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "SHA256") || !strings.Contains(s, "DENIED BY") || !strings.Contains(s, "DENIED AT") || !strings.Contains(s, "REASON") {
		t.Fatalf("denials header = %q", s)
	}
	if !strings.Contains(s, sha1) || !strings.Contains(s, sha2) {
		t.Fatalf("denials output must contain full (copy-pasteable) shas: %q", s)
	}
	if !strings.Contains(s, "malware") || !strings.Contains(s, "admin@example.com") {
		t.Fatalf("denials output = %q", s)
	}
}

func TestRunCatalogDenials_Empty(t *testing.T) {
	f := &fakeCatalogClient{denialsResp: &cpv1.ListSkillObjectDenialsResponse{}}
	var out bytes.Buffer
	if err := runCatalogDenials(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no denied skill objects") {
		t.Fatalf("expected empty-state message, got %q", out.String())
	}
}

// ---- kind helpers ----

func TestParseProfileEntryKind(t *testing.T) {
	cases := []struct {
		s    string
		want cpv1.ProfileEntryKind
	}{
		{"skill", cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL},
		{"mcp", cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP},
		{"config", cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG},
		{"plugin", cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_PLUGIN},
		{"SKILL", cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL},
	}
	for _, tc := range cases {
		got, err := parseProfileEntryKind(tc.s)
		if err != nil {
			t.Fatalf("parseProfileEntryKind(%q): %v", tc.s, err)
		}
		if got != tc.want {
			t.Fatalf("parseProfileEntryKind(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
	if _, err := parseProfileEntryKind("bogus"); err == nil {
		t.Fatal("expected error for bogus kind")
	}
}
