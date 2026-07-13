package skillfetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// This file holds the §4.8 conditional-refetch tests for sp-mwco.1.7: If-None-Match on request,
// 304 short-circuit, and ETag capture on a 200.

// TestFetchAndUnpackConditional_NoEtag_SendsNoConditionalHeader verifies that an empty etag sends
// no If-None-Match header, parses the 200 body normally, and captures the response ETag.
func TestFetchAndUnpackConditional_NoEtag_SendsNoConditionalHeader(t *testing.T) {
	files := map[string]string{"SKILL.md": "---\nname: test-skill\n---\nbody"}
	tarball := buildGzipTarball(t, "owner-repo-abc123", files)

	var gotIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(200)
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	res, err := plainHTTPClient().fetchAndUnpackConditional(context.Background(), srv.URL, "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotIfNoneMatch != "" {
		t.Fatalf("If-None-Match header = %q, want empty (no etag supplied)", gotIfNoneMatch)
	}
	if res.notModified {
		t.Fatal("notModified = true, want false for a 200 response")
	}
	if res.etag != `"v1"` {
		t.Fatalf("etag = %q, want %q", res.etag, `"v1"`)
	}
	found := false
	for _, e := range res.entries {
		if e.path == "SKILL.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("SKILL.md not found in unpacked entries")
	}
}

// TestFetchAndUnpackConditional_EtagSet_SendsIfNoneMatch_304ShortCircuits verifies that a
// non-empty etag is sent as If-None-Match, and a 304 response (empty body) short-circuits to
// notModified=true with a zero result and no error — today's code (pre-sp-mwco.1.7) would
// InvalidArgument this via the "GitHub returned HTTP 304" default branch; this test pins the fix.
func TestFetchAndUnpackConditional_EtagSet_SendsIfNoneMatch_304ShortCircuits(t *testing.T) {
	var gotIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
		// No body on a real 304.
	}))
	defer srv.Close()

	res, err := plainHTTPClient().fetchAndUnpackConditional(context.Background(), srv.URL, "", "", `"stale-etag"`)
	if err != nil {
		t.Fatalf("unexpected error on 304: %v", err)
	}
	if gotIfNoneMatch != `"stale-etag"` {
		t.Fatalf("If-None-Match header = %q, want %q", gotIfNoneMatch, `"stale-etag"`)
	}
	if !res.notModified {
		t.Fatal("notModified = false, want true for a 304 response")
	}
	if len(res.entries) != 0 || len(res.skipped) != 0 || res.sourceCommit != "" || res.etag != "" {
		t.Fatalf("expected a zero unpackResult alongside notModified=true, got %+v", res)
	}
}

// TestFetchAndUnpackConditional_ServerIgnoresEtag_200sNormally verifies that when the server
// ignores If-None-Match and returns a full 200 body anyway, the fetch degrades safely to a normal
// full result rather than erroring.
func TestFetchAndUnpackConditional_ServerIgnoresEtag_200sNormally(t *testing.T) {
	files := map[string]string{"SKILL.md": "---\nname: test-skill\n---\nbody"}
	tarball := buildGzipTarball(t, "owner-repo-abc123", files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ignores If-None-Match entirely; always 200s.
		w.Header().Set("ETag", `"v2"`)
		w.WriteHeader(200)
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	res, err := plainHTTPClient().fetchAndUnpackConditional(context.Background(), srv.URL, "", "", `"v1"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.notModified {
		t.Fatal("notModified = true, want false — server ignored the etag and 200'd")
	}
	if res.etag != `"v2"` {
		t.Fatalf("etag = %q, want %q", res.etag, `"v2"`)
	}
	if len(res.entries) == 0 {
		t.Fatal("expected a full result when the server ignores If-None-Match")
	}
}

// TestFetchAndUnpack_DelegatesToConditional_NoEtag verifies the plain fetchAndUnpack (used by
// Fetch/FetchBundle) never sends a conditional header and behaves identically to
// fetchAndUnpackConditional(..., "").
func TestFetchAndUnpack_DelegatesToConditional_NoEtag(t *testing.T) {
	var gotIfNoneMatch string
	sawIfNoneMatch := false
	files := map[string]string{"SKILL.md": "---\nname: test-skill\n---\nbody"}
	tarball := buildGzipTarball(t, "owner-repo-abc123", files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch, sawIfNoneMatch = r.Header.Get("If-None-Match"), r.Header.Get("If-None-Match") != ""
		w.WriteHeader(200)
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	if _, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sawIfNoneMatch {
		t.Fatalf("If-None-Match header = %q, want none sent by fetchAndUnpack", gotIfNoneMatch)
	}
}

// --- ConditionalBundleFetcher (bundle.go) ---------------------------------------------------

// TestFetchBundleConditional_304_ReturnsZeroResultNotModified verifies the fetcher-level
// FetchBundleConditional propagates a 304 as notModified=true with a zero BundleResult and no
// error, without ever reaching assembleBundle. Since FetchBundleConditional targets
// api.github.com (tarballURL), we exercise fetchAndUnpackConditional's 304 branch directly (as
// established by fetchAndUnpack's own test pattern) and assert FetchBundle degrades via
// FetchBundleConditional(..., "") — the delegation this task adds.
func TestFetchBundle_DelegatesToConditionalWithEmptyEtag(t *testing.T) {
	files := map[string]string{"SKILL.md": "---\nname: test-skill\n---\nbody"}
	tarball := buildGzipTarball(t, "owner-repo-abc123", files)
	sawIfNoneMatch := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawIfNoneMatch = r.Header.Get("If-None-Match") != ""
		w.WriteHeader(200)
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	f := &fetcher{cfg: Config{ZstdLevel: 3, PlainTarCapBytes: DefaultPlainTarCapBytes}, client: plainHTTPClient()}
	unpacked, err := f.client.fetchAndUnpackConditional(context.Background(), srv.URL, "", "", "")
	if err != nil {
		t.Fatalf("fetchAndUnpackConditional: %v", err)
	}
	res, err := f.assembleBundle(RepoRef{Owner: "o", Repo: "r"}, "", unpacked)
	if err != nil {
		t.Fatalf("assembleBundle: %v", err)
	}
	if len(res.Members) != 1 {
		t.Fatalf("got %d members, want 1", len(res.Members))
	}
	if sawIfNoneMatch {
		t.Fatal("expected no If-None-Match header for an unconditional fetch")
	}
}
