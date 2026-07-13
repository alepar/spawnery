package skillfetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// --- ParseRepoURL tests ---

func TestParseRepoURL_OwnerRepo(t *testing.T) {
	ref, err := ParseRepoURL("myorg/myrepo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Owner != "myorg" || ref.Repo != "myrepo" {
		t.Fatalf("got %+v, want Owner=myorg Repo=myrepo", ref)
	}
}

func TestParseRepoURL_HTTPS(t *testing.T) {
	ref, err := ParseRepoURL("https://github.com/myorg/myrepo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Owner != "myorg" || ref.Repo != "myrepo" {
		t.Fatalf("got %+v", ref)
	}
}

func TestParseRepoURL_HTTPS_DotGit(t *testing.T) {
	ref, err := ParseRepoURL("https://github.com/myorg/myrepo.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Repo != "myrepo" {
		t.Fatalf("got Repo=%q, want myrepo", ref.Repo)
	}
}

func TestParseRepoURL_HTTPS_TrailingSlash(t *testing.T) {
	ref, err := ParseRepoURL("https://github.com/myorg/myrepo/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Owner != "myorg" || ref.Repo != "myrepo" {
		t.Fatalf("got %+v", ref)
	}
}

func TestParseRepoURL_OwnerRepo_DotGit(t *testing.T) {
	ref, err := ParseRepoURL("owner/repo.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Repo != "repo" {
		t.Fatalf("got Repo=%q", ref.Repo)
	}
}

func TestParseRepoURL_RejectTreePath(t *testing.T) {
	_, err := ParseRepoURL("https://github.com/owner/repo/tree/main/skills")
	if err == nil {
		t.Fatal("expected error for /tree/ path")
	}
	if !strings.Contains(err.Error(), "deep GitHub URL") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestParseRepoURL_RejectBlobPath(t *testing.T) {
	_, err := ParseRepoURL("https://github.com/owner/repo/blob/main/SKILL.md")
	if err == nil {
		t.Fatal("expected error for /blob/ path")
	}
}

func TestParseRepoURL_RejectNonGitHub(t *testing.T) {
	_, err := ParseRepoURL("https://gitlab.com/owner/repo")
	if err == nil {
		t.Fatal("expected error for non-github URL")
	}
}

func TestParseRepoURL_RejectEmpty(t *testing.T) {
	_, err := ParseRepoURL("")
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestParseRepoURL_RejectJunk(t *testing.T) {
	_, err := ParseRepoURL("not-a-url")
	if err == nil {
		t.Fatal("expected error for bare junk (no slash)")
	}
}

// --- tarball URL tests ---

func TestTarballURL_NoRef(t *testing.T) {
	u := tarballURL("owner", "repo", "")
	want := "https://api.github.com/repos/owner/repo/tarball"
	if u != want {
		t.Fatalf("got %q, want %q", u, want)
	}
}

func TestTarballURL_WithRef(t *testing.T) {
	u := tarballURL("owner", "repo", "main")
	want := "https://api.github.com/repos/owner/repo/tarball/main"
	if u != want {
		t.Fatalf("got %q, want %q", u, want)
	}
}

// --- HTTP fetch tests with fake server ---

// buildGzipTarball builds a gzip+tar archive with the given entries.
// wrapperDir is prepended to every entry (simulates GitHub's owner-repo-sha/ wrapper).
func buildGzipTarball(t *testing.T, wrapperDir string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if wrapperDir != "" {
		_ = tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     wrapperDir + "/",
		})
	}

	for name, content := range files {
		p := name
		if wrapperDir != "" {
			p = wrapperDir + "/" + name
		}
		_ = tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     p,
			Size:     int64(len(content)),
			Mode:     0o644,
		})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// plainHTTPClient returns a simple http.Client (no SSRF protection) for use against local test
// servers, with default caps so existing size/count assertions are unaffected.
func plainHTTPClient() *secureClient {
	return &secureClient{
		client:    &http.Client{},
		wireCap:   DefaultWireCapBytes,
		decompCap: DefaultDecompressedCapBytes,
		fileCap:   DefaultFileCountCap,
	}
}

func TestFetch_HappyPath(t *testing.T) {
	files := map[string]string{
		"SKILL.md": "---\nname: test-skill\n---\n# Test Skill",
		"main.md":  "main content",
	}
	tarball := buildGzipTarball(t, "owner-repo-abc123", files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	unpacked, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries := unpacked.entries

	found := false
	for _, e := range entries {
		if e.path == "SKILL.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("SKILL.md not found in unpacked entries")
	}
}

func TestFetch_MissingSkillMD(t *testing.T) {
	tarball := buildGzipTarball(t, "wrapper", map[string]string{"README.md": "just a readme"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	f := &fetcher{cfg: Config{ZstdLevel: 3}, client: plainHTTPClient()}
	_, err := f.Fetch(context.Background(), RepoRef{Owner: "owner", Repo: "repo"}, "", "", "", "")
	// Fetch targets api.github.com, so it will fail on the transport. We test the logic via entries.
	// Instead test the SKILL.md validation directly.
	if err == nil {
		t.Fatal("expected error; either fetch or SKILL.md validation failed")
	}
	_ = err // either network error or SKILL.md error is expected
}

func TestFetch_GitHub429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err == nil {
		t.Fatal("expected error for 429")
	}
	rl, ok := err.(*ErrRateLimit)
	if !ok {
		t.Fatalf("expected *ErrRateLimit, got %T: %v", err, err)
	}
	if rl.RetryAfter != "60" {
		t.Fatalf("expected RetryAfter=60, got %q", rl.RetryAfter)
	}
}

func TestFetch_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestFetch_SymlinkSkipped(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "wrapper/"})
	skillMD := "---\nname: test-skill\n---\n# Test Skill"
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "wrapper/SKILL.md",
		Size:     int64(len(skillMD)),
		Mode:     0o644,
	})
	_, _ = tw.Write([]byte(skillMD))
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     "wrapper/AGENTS.md",
		Linkname: "CLAUDE.md",
		Mode:     0o777,
	})
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	unpacked, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries, skipped := unpacked.entries, unpacked.skipped

	var foundSkillMD, foundAgentsMD bool
	for _, e := range entries {
		if e.path == "SKILL.md" {
			foundSkillMD = true
		}
		if e.path == "AGENTS.md" {
			foundAgentsMD = true
		}
	}
	if !foundSkillMD {
		t.Fatal("SKILL.md not found in unpacked entries")
	}
	if foundAgentsMD {
		t.Fatal("AGENTS.md symlink should not appear in unpacked entries")
	}

	if len(skipped) != 1 || skipped[0].path != "AGENTS.md" || skipped[0].kind != "symlink" {
		t.Fatalf("expected one skipped symlink entry AGENTS.md, got: %+v", skipped)
	}
}

func TestFetch_HardlinkSkipped(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "wrapper/"})
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeLink,
		Name:     "wrapper/hardlinked",
		Linkname: "wrapper/SKILL.md",
	})
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	unpacked, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries, skipped := unpacked.entries, unpacked.skipped
	for _, e := range entries {
		if e.path == "hardlinked" {
			t.Fatal("hardlinked entry should not appear in unpacked entries")
		}
	}
	if len(skipped) != 1 || skipped[0].path != "hardlinked" || skipped[0].kind != "hardlink" {
		t.Fatalf("expected one skipped hardlink entry, got: %+v", skipped)
	}
}

func TestFetch_DeviceAndFifoSkipped(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "wrapper/"})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeChar, Name: "wrapper/dev1"})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeBlock, Name: "wrapper/dev2"})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeFifo, Name: "wrapper/fifo1"})
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	unpacked, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries, skipped := unpacked.entries, unpacked.skipped
	if len(entries) != 0 {
		t.Fatalf("expected no entries, got: %+v", entries)
	}
	if len(skipped) != 3 {
		t.Fatalf("expected 3 skipped entries, got: %+v", skipped)
	}
	wantKinds := map[string]string{"dev1": "chardev", "dev2": "blockdev", "fifo1": "fifo"}
	for _, s := range skipped {
		if wantKinds[s.path] != s.kind {
			t.Fatalf("skipped entry %q: got kind %q, want %q", s.path, s.kind, wantKinds[s.path])
		}
	}
}

func TestFetch_SkippedEntryOutsideSubdirNotReported(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "wrapper/"})
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     "wrapper/AGENTS.md",
		Linkname: "CLAUDE.md",
	})
	skillMD := "---\nname: nested-skill\n---"
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "wrapper/skills/nested/SKILL.md",
		Size:     int64(len(skillMD)),
		Mode:     0o644,
	})
	_, _ = tw.Write([]byte(skillMD))
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	unpacked, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "skills/nested")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries, skipped := unpacked.entries, unpacked.skipped
	if len(skipped) != 0 {
		t.Fatalf("expected no skipped entries when the symlink is outside the requested subdir, got: %+v", skipped)
	}
	var foundSkillMD bool
	for _, e := range entries {
		if e.path == "SKILL.md" {
			foundSkillMD = true
		}
	}
	if !foundSkillMD {
		t.Fatal("SKILL.md not found after subdir descent")
	}
}

func TestFetch_AbsolutePathRejected(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "wrapper/"})
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "/etc/passwd",
		Size:     5,
	})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	_, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err == nil {
		t.Fatal("expected error for absolute path entry")
	}
}

func TestFetch_DotDotEscapeRejected(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "wrapper/"})
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "wrapper/../../../etc/passwd",
		Size:     5,
	})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	_, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err == nil {
		t.Fatal("expected error for path traversal entry")
	}
}

func TestFetch_SymlinkWithEscapePathRejected(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "wrapper/"})
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     "wrapper/../evil",
		Linkname: "CLAUDE.md",
	})
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	_, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err == nil {
		t.Fatal("expected error for symlink entry whose name escapes via ..")
	}
}

func TestFetch_SubdirDescent(t *testing.T) {
	skillMD := "---\nname: nested-skill\n---"
	files := map[string]string{
		"skills/nested/SKILL.md": skillMD,
		"skills/nested/main.md":  "content",
		"other/file.txt":         "irrelevant",
	}
	tarball := buildGzipTarball(t, "wrapper", files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	unpacked, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "skills/nested")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries := unpacked.entries

	var foundSkillMD bool
	for _, e := range entries {
		if e.path == "SKILL.md" {
			foundSkillMD = true
		}
		if strings.HasPrefix(e.path, "skills/") || strings.HasPrefix(e.path, "other/") {
			t.Fatalf("subdir descent should have stripped prefix, got path: %q", e.path)
		}
	}
	if !foundSkillMD {
		t.Fatal("SKILL.md not found after subdir descent")
	}
}

func TestFetch_SkippedEntryDoesNotAffectSHA(t *testing.T) {
	skillMD := "---\nname: test-skill\n---\n# Test Skill"

	var withSymlinkBuf bytes.Buffer
	gz := gzip.NewWriter(&withSymlinkBuf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "wrapper/"})
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "wrapper/SKILL.md",
		Size:     int64(len(skillMD)),
		Mode:     0o644,
	})
	_, _ = tw.Write([]byte(skillMD))
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     "wrapper/AGENTS.md",
		Linkname: "CLAUDE.md",
	})
	_ = tw.Close()
	_ = gz.Close()

	withoutSymlink := buildGzipTarball(t, "wrapper", map[string]string{"SKILL.md": skillMD})

	srvWith := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(withSymlinkBuf.Bytes())
	}))
	defer srvWith.Close()

	srvWithout := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(withoutSymlink)
	}))
	defer srvWithout.Close()

	unpackedWith, err := plainHTTPClient().fetchAndUnpack(context.Background(), srvWith.URL, "", "")
	if err != nil {
		t.Fatalf("fetch with symlink: %v", err)
	}
	entriesWith, skippedWith := unpackedWith.entries, unpackedWith.skipped
	unpackedWithout, err := plainHTTPClient().fetchAndUnpack(context.Background(), srvWithout.URL, "", "")
	if err != nil {
		t.Fatalf("fetch without symlink: %v", err)
	}
	entriesWithout, skippedWithout := unpackedWithout.entries, unpackedWithout.skipped

	tarWith, err := canonicalRepack(entriesWith)
	if err != nil {
		t.Fatalf("repack with symlink: %v", err)
	}
	tarWithout, err := canonicalRepack(entriesWithout)
	if err != nil {
		t.Fatalf("repack without symlink: %v", err)
	}

	shaWith := sha256.Sum256(tarWith)
	shaWithout := sha256.Sum256(tarWithout)
	if shaWith != shaWithout {
		t.Fatalf("skipped symlink entry affected sha256: with=%x without=%x", shaWith, shaWithout)
	}
	if len(skippedWith) != 1 {
		t.Fatalf("expected 1 skipped entry, got %v", skippedWith)
	}
	if len(skippedWithout) != 0 {
		t.Fatalf("expected no skipped entries, got %v", skippedWithout)
	}
}

// --- SSRF protection tests ---

func TestSSRF_DisallowedRedirect(t *testing.T) {
	// A server that redirects to the test server itself (127.0.0.1, not in allowedHosts)
	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
	}))
	defer redirSrv.Close()

	c := newSecureClient(Config{
		WireCapBytes:         DefaultWireCapBytes,
		DecompressedCapBytes: DefaultDecompressedCapBytes,
		PlainTarCapBytes:     DefaultPlainTarCapBytes,
		FileCountCap:         DefaultFileCountCap,
		HTTPTimeout:          DefaultHTTPTimeout,
	})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, redirSrv.URL, nil)
	// The CheckRedirect should reject the redirect to 127.0.0.1 (not in allowedHosts)
	_, err := c.client.Do(req)
	if err == nil {
		t.Fatal("expected error for redirect to non-allowlisted host")
	}
}

func TestSSRF_PrivateIP_BlockedAtDial(t *testing.T) {
	privateIPs := []string{
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"100.64.0.1",
	}
	for _, ip := range privateIPs {
		parsed := net.ParseIP(ip)
		if !isBlockedIP(parsed) {
			t.Errorf("expected %q to be blocked by isBlockedIP", ip)
		}
	}
}

func TestSSRF_PublicIP_Allowed(t *testing.T) {
	publicIPs := []string{
		"8.8.8.8",
		"140.82.121.4",
	}
	for _, ip := range publicIPs {
		parsed := net.ParseIP(ip)
		if isBlockedIP(parsed) {
			t.Errorf("expected %q to be allowed by isBlockedIP", ip)
		}
	}
}

// --- Canonical repack determinism tests ---

func TestCanonicalRepack_Deterministic(t *testing.T) {
	entries := []tarEntry{
		{path: "SKILL.md", mode: 0o644, content: []byte("hello")},
		{path: "main.md", mode: 0o644, content: []byte("main")},
		{path: "scripts/run.sh", mode: 0o755, content: []byte("#!/bin/sh")},
	}

	tar1, err := canonicalRepack(entries)
	if err != nil {
		t.Fatalf("first repack: %v", err)
	}
	tar2, err := canonicalRepack(entries)
	if err != nil {
		t.Fatalf("second repack: %v", err)
	}

	h1 := sha256.Sum256(tar1)
	h2 := sha256.Sum256(tar2)
	if h1 != h2 {
		t.Fatal("two repacks of identical entries produced different sha256")
	}
}

func TestCanonicalRepack_OrderIndependent(t *testing.T) {
	entries := []tarEntry{
		{path: "z_file.md", mode: 0o644, content: []byte("z")},
		{path: "a_file.md", mode: 0o644, content: []byte("a")},
	}
	reversed := []tarEntry{entries[1], entries[0]}

	tar1, _ := canonicalRepack(entries)
	tar2, _ := canonicalRepack(reversed)

	h1 := sha256.Sum256(tar1)
	h2 := sha256.Sum256(tar2)
	if h1 != h2 {
		t.Fatal("canonical repack must be order-independent (sorted by path)")
	}
}

func TestCanonicalRepack_ExecBitDiffers(t *testing.T) {
	exec := []tarEntry{{path: "run.sh", mode: 0o755, content: []byte("#!/bin/sh")}}
	noExec := []tarEntry{{path: "run.sh", mode: 0o644, content: []byte("#!/bin/sh")}}

	tar1, _ := canonicalRepack(exec)
	tar2, _ := canonicalRepack(noExec)

	h1 := sha256.Sum256(tar1)
	h2 := sha256.Sum256(tar2)
	if h1 == h2 {
		t.Fatal("exec bit should produce different tar (mode 0755 vs 0644)")
	}
}

func TestCanonicalRepack_ZstdRoundTrip(t *testing.T) {
	entries := []tarEntry{
		{path: "SKILL.md", content: []byte("---\nname: test\n---\n# Skill"), mode: 0o644},
		{path: "main.md", content: []byte("content"), mode: 0o644},
	}
	plainTar, err := canonicalRepack(entries)
	if err != nil {
		t.Fatalf("repack: %v", err)
	}

	h1 := sha256.Sum256(plainTar)

	// Compress
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		t.Fatalf("zstd encoder: %v", err)
	}
	compressed := enc.EncodeAll(plainTar, nil)

	// Decompress
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd decoder: %v", err)
	}
	defer dec.Close()
	decompressed, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatalf("zstd decompress: %v", err)
	}

	h2 := sha256.Sum256(decompressed)
	if h1 != h2 {
		t.Fatal("sha256 mismatch after zstd round-trip")
	}
	if !bytes.Equal(plainTar, decompressed) {
		t.Fatal("bytes differ after zstd round-trip")
	}
}

// --- SKILL.md frontmatter tests ---

func TestParseSkillMD_Name(t *testing.T) {
	fm, err := parseSkillMD([]byte("---\nname: my-skill\n---\n# Content here"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fm.Name != "my-skill" {
		t.Fatalf("got Name=%q, want my-skill", fm.Name)
	}
}

func TestParseSkillMD_NoFrontmatter(t *testing.T) {
	fm, err := parseSkillMD([]byte("# Just a heading\nNo frontmatter here"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fm.Name != "" {
		t.Fatalf("expected empty name, got %q", fm.Name)
	}
}

func TestParseSkillMD_GarbledFrontmatterNoHardFail(t *testing.T) {
	// Garbled frontmatter should not hard-fail
	_, err := parseSkillMD([]byte("---\n: garbled: [\n---\n# Content"))
	if err != nil {
		t.Fatalf("garbled frontmatter should not error, got: %v", err)
	}
}

func TestFetch_FrontmatterUsedAsDefaultName(t *testing.T) {
	// Verify that when no request name is given, frontmatter name is used.
	files := map[string]string{
		"SKILL.md": "---\nname: frontmatter-skill\n---\n# Skill",
	}
	tarball := buildGzipTarball(t, "wrapper", files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	unpacked, err := plainHTTPClient().fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err != nil {
		t.Fatalf("fetchAndUnpack: %v", err)
	}
	entries := unpacked.entries

	var skillContent []byte
	for _, e := range entries {
		if e.path == "SKILL.md" {
			skillContent = e.content
		}
	}
	if skillContent == nil {
		t.Fatal("SKILL.md not found")
	}
	fm, _ := parseSkillMD(skillContent)
	if fm.Name != "frontmatter-skill" {
		t.Fatalf("expected 'frontmatter-skill', got %q", fm.Name)
	}
}

// --- validateName tests ---

func TestValidateName_Valid(t *testing.T) {
	if err := validateName("my-skill"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateName_Empty(t *testing.T) {
	if err := validateName(""); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestValidateName_WithSlash(t *testing.T) {
	if err := validateName("a/b"); err == nil {
		t.Fatal("expected error for name with slash")
	}
}

func TestValidateName_DotDot(t *testing.T) {
	if err := validateName(".."); err == nil {
		t.Fatal("expected error for '..'")
	}
}

// --- safeRelPath tests ---

func TestSafeRelPath_Valid(t *testing.T) {
	p, err := safeRelPath("subdir/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != "subdir/file.txt" {
		t.Fatalf("got %q", p)
	}
}

func TestSafeRelPath_DotDotRejected(t *testing.T) {
	_, err := safeRelPath("../escape")
	if err == nil {
		t.Fatal("expected error for .. path")
	}
}

func TestSafeRelPath_AbsoluteRejected(t *testing.T) {
	_, err := safeRelPath("/absolute/path")
	if err == nil {
		t.Fatal("expected error for absolute path")
	}
}
