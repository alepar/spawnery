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

// fakeBundleClient is a canned bundleClient that records each request.
type fakeBundleClient struct {
	listResp     *cpv1.ListBundlesResponse
	getResp      *cpv1.GetBundleResponse
	reingestResp *cpv1.ReingestBundleResponse
	reingestErr  error

	gotGet      *cpv1.GetBundleRequest
	gotReingest *cpv1.ReingestBundleRequest
}

func (f *fakeBundleClient) ListBundles(_ context.Context, _ *connect.Request[cpv1.ListBundlesRequest]) (*connect.Response[cpv1.ListBundlesResponse], error) {
	return connect.NewResponse(f.listResp), nil
}

func (f *fakeBundleClient) GetBundle(_ context.Context, r *connect.Request[cpv1.GetBundleRequest]) (*connect.Response[cpv1.GetBundleResponse], error) {
	f.gotGet = r.Msg
	return connect.NewResponse(f.getResp), nil
}

func (f *fakeBundleClient) ReingestBundle(_ context.Context, r *connect.Request[cpv1.ReingestBundleRequest]) (*connect.Response[cpv1.ReingestBundleResponse], error) {
	f.gotReingest = r.Msg
	if f.reingestErr != nil {
		return nil, f.reingestErr
	}
	return connect.NewResponse(f.reingestResp), nil
}

// ---- list ----

func TestRunBundleList(t *testing.T) {
	f := &fakeBundleClient{listResp: &cpv1.ListBundlesResponse{Bundles: []*cpv1.BundleSummary{
		{
			BundleId:     "bun-1",
			Name:         "superpowers",
			SourceUrl:    "https://github.com/obra/superpowers",
			SourceRef:    "main",
			SourceSubdir: "skills",
			LatestSeq:    3,
			MemberCount:  5,
			UpdatedAt:    1700000000,
		},
	}}}
	var out bytes.Buffer
	if err := runBundleList(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"BUNDLE ID", "NAME", "SOURCE", "REF", "SUBDIR", "VERSION", "MEMBERS", "UPDATED",
		"bun-1", "superpowers", "https://github.com/obra/superpowers", "main", "skills", "v3", "5",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("list output missing %q; got:\n%s", want, s)
		}
	}
}

func TestRunBundleList_Empty(t *testing.T) {
	f := &fakeBundleClient{listResp: &cpv1.ListBundlesResponse{}}
	var out bytes.Buffer
	if err := runBundleList(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no bundles") {
		t.Fatalf("expected empty-state message, got %q", out.String())
	}
}

// ---- show ----

func TestRunBundleShow(t *testing.T) {
	f := &fakeBundleClient{getResp: &cpv1.GetBundleResponse{
		Bundle: &cpv1.BundleSummary{
			BundleId:     "bun-1",
			Name:         "superpowers",
			SourceUrl:    "https://github.com/obra/superpowers",
			SourceRef:    "main",
			SourceSubdir: "skills",
			CreatedAt:    1600000000,
			UpdatedAt:    1700000000,
			LatestSeq:    2,
		},
		Versions: []*cpv1.BundleVersion{
			{VersionId: "ver-1", Seq: 1, SourceCommit: "1111111111111111111111111111111111aaaa", CreatedAt: 1600000000},
			{VersionId: "ver-2", Seq: 2, SourceCommit: "2222222222222222222222222222222222bbbb", CreatedAt: 1700000000},
		},
		Members: []*cpv1.BundleMember{
			{CatalogId: "cat-1", SourceSubdir: "skills/a", Name: "a", Description: "skill a", Sha256: "3333333333333333333333333333333333333333333333333333333333cccc", Position: 0},
			{CatalogId: "cat-2", SourceSubdir: "skills/b", Name: "b", Description: "skill b", Sha256: "4444444444444444444444444444444444444444444444444444444444dddd", Position: 1},
		},
	}}
	var out bytes.Buffer
	if err := runBundleShow(context.Background(), f, &out, "bun-1"); err != nil {
		t.Fatal(err)
	}
	if f.gotGet.GetBundleId() != "bun-1" {
		t.Fatalf("get bundle request = %+v", f.gotGet)
	}
	s := out.String()
	for _, want := range []string{
		"bun-1", "superpowers", "https://github.com/obra/superpowers", "main", "skills",
		"SEQ", "VERSION ID", "COMMIT", "CREATED",
		"ver-1", "ver-2", "111111111111", "222222222222",
		"Members (latest version v2):",
		"#", "NAME", "SUBDIR", "CATALOG ID", "SHA256", "DESCRIPTION",
		"skills/a", "a", "skill a", "cat-1",
		"skills/b", "b", "skill b", "cat-2",
		// the member sha must be printed in FULL (not shortHash-truncated like the version commit
		// column above) so it can be pasted straight into `catalog deny <sha>` (sp-mwco.3.6).
		"3333333333333333333333333333333333333333333333333333333333cccc",
		"4444444444444444444444444444444444444444444444444444444444dddd",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("show output missing %q; got:\n%s", want, s)
		}
	}
}

// ---- reingest ----

func TestRunBundleReingest_NotModified(t *testing.T) {
	f := &fakeBundleClient{reingestResp: &cpv1.ReingestBundleResponse{
		VersionId:   "ver-2",
		NotModified: true,
	}}
	var out bytes.Buffer
	if err := runBundleReingest(context.Background(), f, &out, "bun-1"); err != nil {
		t.Fatal(err)
	}
	if f.gotReingest.GetBundleId() != "bun-1" {
		t.Fatalf("reingest request = %+v", f.gotReingest)
	}
	s := out.String()
	if !strings.Contains(s, "up to date") {
		t.Fatalf("expected 'up to date' message, got %q", s)
	}
	if strings.Contains(s, "diff_token") || strings.Contains(s, "diff-token") {
		t.Fatalf("not_modified output should not mention a diff token: %q", s)
	}
}

func TestRunBundleReingest_Unchanged(t *testing.T) {
	f := &fakeBundleClient{reingestResp: &cpv1.ReingestBundleResponse{
		VersionId: "ver-2",
		Changed:   false,
	}}
	var out bytes.Buffer
	if err := runBundleReingest(context.Background(), f, &out, "bun-1"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "no new version") {
		t.Fatalf("expected 'no new version' message, got %q", s)
	}
}

func TestRunBundleReingest_Changed(t *testing.T) {
	f := &fakeBundleClient{reingestResp: &cpv1.ReingestBundleResponse{
		VersionId:      "ver-3",
		Changed:        true,
		AddedSubdirs:   []string{"skills/c"},
		UpdatedSubdirs: []string{"skills/a"},
		RemovedSubdirs: []string{"skills/b"},
		DiffToken:      "tok-abc123",
		FromVersionId:  "ver-2",
		Warnings:       []string{"nested skill skipped at skills/d/nested"},
	}}
	var out bytes.Buffer
	if err := runBundleReingest(context.Background(), f, &out, "bun-1"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"ver-3", "skills/c", "skills/a", "skills/b", "tok-abc123",
		"nested skill skipped at skills/d/nested",
		"diff", // hint that a diff view is required before re-pin
	} {
		if !strings.Contains(s, want) {
			t.Errorf("reingest output missing %q; got:\n%s", want, s)
		}
	}
}

func TestRunBundleReingest_Changed_OmitsEmptySections(t *testing.T) {
	f := &fakeBundleClient{reingestResp: &cpv1.ReingestBundleResponse{
		VersionId: "ver-3",
		Changed:   true,
		DiffToken: "tok-xyz",
	}}
	var out bytes.Buffer
	if err := runBundleReingest(context.Background(), f, &out, "bun-1"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Contains(s, "Added") || strings.Contains(s, "Updated") || strings.Contains(s, "Removed") || strings.Contains(s, "Warning") {
		t.Fatalf("expected empty sections to be skipped, got %q", s)
	}
}

func TestRunBundleReingest_ErrorPropagates(t *testing.T) {
	f := &fakeBundleClient{reingestErr: connect.NewError(connect.CodeResourceExhausted, errors.New("refetch budget exhausted"))}
	var out bytes.Buffer
	err := runBundleReingest(context.Background(), f, &out, "bun-1")
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}
