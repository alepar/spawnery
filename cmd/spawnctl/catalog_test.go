package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
)

// fakeCatalogClient is a canned catalogClient that records each request.
type fakeCatalogClient struct {
	createResp *cpv1.CreateCatalogEntryResponse
	getResp    *cpv1.GetCatalogEntryResponse
	listResp   *cpv1.ListCatalogEntriesResponse

	gotCreate  *cpv1.CreateCatalogEntryRequest
	gotUpdate  *cpv1.UpdateCatalogEntryRequest
	gotDelete  *cpv1.DeleteCatalogEntryRequest
	gotListing *cpv1.SetCatalogListingRequest
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
