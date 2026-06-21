package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
)

// fakeProfileClient is a canned profileClient that records each request.
type fakeProfileClient struct {
	createResp    *cpv1.CreateProfileResponse
	getResp       *cpv1.GetProfileResponse
	listResp      *cpv1.ListProfilesResponse
	updateResp    *cpv1.UpdateProfileResponse
	addEntryResp  *cpv1.AddProfileEntryResponse
	addSecretResp *cpv1.AddProfileSecretRefResponse

	gotCreate       *cpv1.CreateProfileRequest
	gotGet          *cpv1.GetProfileRequest
	gotUpdate       *cpv1.UpdateProfileRequest
	gotDelete       *cpv1.DeleteProfileRequest
	gotAddEntry     *cpv1.AddProfileEntryRequest
	gotRemoveEntry  *cpv1.RemoveProfileEntryRequest
	gotAddSecret    *cpv1.AddProfileSecretRefRequest
	gotRemoveSecret *cpv1.RemoveProfileSecretRefRequest

	getCallCount int
}

func (f *fakeProfileClient) CreateProfile(_ context.Context, r *connect.Request[cpv1.CreateProfileRequest]) (*connect.Response[cpv1.CreateProfileResponse], error) {
	f.gotCreate = r.Msg
	return connect.NewResponse(f.createResp), nil
}

func (f *fakeProfileClient) GetProfile(_ context.Context, r *connect.Request[cpv1.GetProfileRequest]) (*connect.Response[cpv1.GetProfileResponse], error) {
	f.gotGet = r.Msg
	f.getCallCount++
	return connect.NewResponse(f.getResp), nil
}

func (f *fakeProfileClient) ListProfiles(_ context.Context, _ *connect.Request[cpv1.ListProfilesRequest]) (*connect.Response[cpv1.ListProfilesResponse], error) {
	return connect.NewResponse(f.listResp), nil
}

func (f *fakeProfileClient) UpdateProfile(_ context.Context, r *connect.Request[cpv1.UpdateProfileRequest]) (*connect.Response[cpv1.UpdateProfileResponse], error) {
	f.gotUpdate = r.Msg
	return connect.NewResponse(f.updateResp), nil
}

func (f *fakeProfileClient) DeleteProfile(_ context.Context, r *connect.Request[cpv1.DeleteProfileRequest]) (*connect.Response[cpv1.DeleteProfileResponse], error) {
	f.gotDelete = r.Msg
	return connect.NewResponse(&cpv1.DeleteProfileResponse{}), nil
}

func (f *fakeProfileClient) AddProfileEntry(_ context.Context, r *connect.Request[cpv1.AddProfileEntryRequest]) (*connect.Response[cpv1.AddProfileEntryResponse], error) {
	f.gotAddEntry = r.Msg
	return connect.NewResponse(f.addEntryResp), nil
}

func (f *fakeProfileClient) RemoveProfileEntry(_ context.Context, r *connect.Request[cpv1.RemoveProfileEntryRequest]) (*connect.Response[cpv1.RemoveProfileEntryResponse], error) {
	f.gotRemoveEntry = r.Msg
	return connect.NewResponse(&cpv1.RemoveProfileEntryResponse{Version: 5}), nil
}

func (f *fakeProfileClient) AddProfileSecretRef(_ context.Context, r *connect.Request[cpv1.AddProfileSecretRefRequest]) (*connect.Response[cpv1.AddProfileSecretRefResponse], error) {
	f.gotAddSecret = r.Msg
	return connect.NewResponse(f.addSecretResp), nil
}

func (f *fakeProfileClient) RemoveProfileSecretRef(_ context.Context, r *connect.Request[cpv1.RemoveProfileSecretRefRequest]) (*connect.Response[cpv1.RemoveProfileSecretRefResponse], error) {
	f.gotRemoveSecret = r.Msg
	return connect.NewResponse(&cpv1.RemoveProfileSecretRefResponse{Version: 7}), nil
}

// ---- create ----

func TestRunProfileCreate(t *testing.T) {
	f := &fakeProfileClient{
		createResp: &cpv1.CreateProfileResponse{ProfileId: "prof-1", Version: 1},
	}
	var out bytes.Buffer
	if err := runProfileCreate(context.Background(), f, &out, "demo"); err != nil {
		t.Fatal(err)
	}
	if f.gotCreate.GetName() != "demo" {
		t.Fatalf("create req name = %q, want %q", f.gotCreate.GetName(), "demo")
	}
	if !strings.Contains(out.String(), "prof-1") {
		t.Fatalf("output missing profile id: %q", out.String())
	}
}

// ---- list ----

func TestRunProfileList(t *testing.T) {
	f := &fakeProfileClient{
		listResp: &cpv1.ListProfilesResponse{
			Profiles: []*cpv1.ProfileSummary{
				{ProfileId: "prof-1", Name: "demo", Version: 3},
			},
		},
	}
	var out bytes.Buffer
	if err := runProfileList(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "prof-1") || !strings.Contains(out.String(), "demo") {
		t.Fatalf("list output = %q", out.String())
	}
}

// ---- show ----

func TestRunProfileShow(t *testing.T) {
	f := &fakeProfileClient{
		getResp: &cpv1.GetProfileResponse{
			Profile: &cpv1.Profile{
				ProfileId: "prof-1",
				Name:      "demo",
				Version:   3,
				Entries: []*cpv1.ProfileEntry{
					{
						EntryId:   "e-1",
						Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
						Name:      "my-skill",
						Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
						CatalogId: "cat-abc",
					},
				},
				SecretIds: []string{"sec-1"},
			},
		},
	}
	var out bytes.Buffer
	if err := runProfileShow(context.Background(), f, &out, "prof-1"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "prof-1") || !strings.Contains(s, "my-skill") || !strings.Contains(s, "sec-1") {
		t.Fatalf("show output = %q", s)
	}
}

// ---- rename: version resolved via GetProfile ----

func TestRunProfileRenameFetchesVersion(t *testing.T) {
	f := &fakeProfileClient{
		getResp:    &cpv1.GetProfileResponse{Profile: &cpv1.Profile{ProfileId: "p-1", Version: 4}},
		updateResp: &cpv1.UpdateProfileResponse{Version: 5},
	}
	var out bytes.Buffer
	if err := runProfileRename(context.Background(), f, &out, "p-1", "newname", 0); err != nil {
		t.Fatal(err)
	}
	// GetProfile must have been called to resolve the version.
	if f.getCallCount != 1 {
		t.Fatalf("expected 1 GetProfile call, got %d", f.getCallCount)
	}
	if f.gotUpdate.GetExpectedVersion() != 4 {
		t.Fatalf("expected version 4 in UpdateProfile, got %d", f.gotUpdate.GetExpectedVersion())
	}
	if f.gotUpdate.GetName() != "newname" {
		t.Fatalf("update name = %q, want newname", f.gotUpdate.GetName())
	}
}

func TestRunProfileRenameExplicitVersionSkipsGet(t *testing.T) {
	f := &fakeProfileClient{
		updateResp: &cpv1.UpdateProfileResponse{Version: 10},
	}
	var out bytes.Buffer
	if err := runProfileRename(context.Background(), f, &out, "p-1", "renamed", 9); err != nil {
		t.Fatal(err)
	}
	// With explicit version, GetProfile must NOT be called.
	if f.getCallCount != 0 {
		t.Fatalf("expected 0 GetProfile calls, got %d", f.getCallCount)
	}
	if f.gotUpdate.GetExpectedVersion() != 9 {
		t.Fatalf("expected version 9, got %d", f.gotUpdate.GetExpectedVersion())
	}
}

// ---- delete ----

func TestRunProfileDelete(t *testing.T) {
	f := &fakeProfileClient{}
	var out bytes.Buffer
	if err := runProfileDelete(context.Background(), f, &out, "p-del"); err != nil {
		t.Fatal(err)
	}
	if f.gotDelete.GetProfileId() != "p-del" {
		t.Fatalf("delete req id = %q, want p-del", f.gotDelete.GetProfileId())
	}
	if !strings.Contains(out.String(), "p-del") {
		t.Fatalf("output missing id: %q", out.String())
	}
}

// ---- entry add: catalog source ----

func TestRunProfileEntryAddCatalog(t *testing.T) {
	f := &fakeProfileClient{
		getResp:      &cpv1.GetProfileResponse{Profile: &cpv1.Profile{ProfileId: "p-1", Version: 2}},
		addEntryResp: &cpv1.AddProfileEntryResponse{EntryId: "e-99", Version: 3},
	}
	var out bytes.Buffer
	p := entryAddParams{
		Kind:      "skill",
		Name:      "my-skill",
		CatalogID: "cat-abc",
	}
	if err := runProfileEntryAdd(context.Background(), f, &out, "p-1", p); err != nil {
		t.Fatal(err)
	}
	req := f.gotAddEntry
	if req.GetEntry().GetKind() != cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL {
		t.Fatalf("kind = %v", req.GetEntry().GetKind())
	}
	if req.GetEntry().GetSource() != cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF {
		t.Fatalf("source = %v", req.GetEntry().GetSource())
	}
	if req.GetEntry().GetCatalogId() != "cat-abc" {
		t.Fatalf("catalog id = %q", req.GetEntry().GetCatalogId())
	}
	if req.GetExpectedVersion() != 2 {
		t.Fatalf("expected version = %d", req.GetExpectedVersion())
	}
	if !strings.Contains(out.String(), "e-99") {
		t.Fatalf("output = %q", out.String())
	}
}

// ---- entry add: custom inline source ----

func TestRunProfileEntryAddCustomInline(t *testing.T) {
	f := &fakeProfileClient{
		getResp:      &cpv1.GetProfileResponse{Profile: &cpv1.Profile{Version: 1}},
		addEntryResp: &cpv1.AddProfileEntryResponse{EntryId: "e-custom", Version: 2},
	}
	var out bytes.Buffer
	p := entryAddParams{
		Kind:         "mcp",
		Name:         "my-mcp",
		CustomInline: []byte("content here"),
	}
	if err := runProfileEntryAdd(context.Background(), f, &out, "p-2", p); err != nil {
		t.Fatal(err)
	}
	req := f.gotAddEntry
	if req.GetEntry().GetSource() != cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM {
		t.Fatalf("source = %v", req.GetEntry().GetSource())
	}
	if string(req.GetEntry().GetCustomInline()) != "content here" {
		t.Fatalf("custom inline = %q", req.GetEntry().GetCustomInline())
	}
}

// ---- entry add: reject both sources ----

func TestRunProfileEntryAddRejectsBothSources(t *testing.T) {
	f := &fakeProfileClient{}
	var out bytes.Buffer
	p := entryAddParams{
		Kind:         "skill",
		Name:         "n",
		CatalogID:    "cat-1",
		CustomInline: []byte("x"),
	}
	err := runProfileEntryAdd(context.Background(), f, &out, "p-1", p)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
}

// ---- entry add: reject no source ----

func TestRunProfileEntryAddRejectsNoSource(t *testing.T) {
	f := &fakeProfileClient{}
	var out bytes.Buffer
	p := entryAddParams{Kind: "skill", Name: "n"}
	err := runProfileEntryAdd(context.Background(), f, &out, "p-1", p)
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required error, got %v", err)
	}
}

// ---- entry remove ----

func TestRunProfileEntryRemove(t *testing.T) {
	f := &fakeProfileClient{
		getResp: &cpv1.GetProfileResponse{Profile: &cpv1.Profile{Version: 5}},
	}
	var out bytes.Buffer
	if err := runProfileEntryRemove(context.Background(), f, &out, "p-1", "e-42", 0); err != nil {
		t.Fatal(err)
	}
	if f.gotRemoveEntry.GetEntryId() != "e-42" {
		t.Fatalf("remove entry id = %q", f.gotRemoveEntry.GetEntryId())
	}
	if f.gotRemoveEntry.GetExpectedVersion() != 5 {
		t.Fatalf("remove expected version = %d", f.gotRemoveEntry.GetExpectedVersion())
	}
}

// ---- secret add/remove ----

func TestRunProfileSecretAddRemove(t *testing.T) {
	f := &fakeProfileClient{
		getResp:       &cpv1.GetProfileResponse{Profile: &cpv1.Profile{Version: 3}},
		addSecretResp: &cpv1.AddProfileSecretRefResponse{Version: 4},
	}
	var out bytes.Buffer
	if err := runProfileSecretAdd(context.Background(), f, &out, "p-1", "sec-xyz", 0); err != nil {
		t.Fatal(err)
	}
	if f.gotAddSecret.GetSecretId() != "sec-xyz" {
		t.Fatalf("add secret id = %q", f.gotAddSecret.GetSecretId())
	}

	f.getResp = &cpv1.GetProfileResponse{Profile: &cpv1.Profile{Version: 4}}
	var out2 bytes.Buffer
	if err := runProfileSecretRemove(context.Background(), f, &out2, "p-1", "sec-xyz", 0); err != nil {
		t.Fatal(err)
	}
	if f.gotRemoveSecret.GetSecretId() != "sec-xyz" {
		t.Fatalf("remove secret id = %q", f.gotRemoveSecret.GetSecretId())
	}
}
