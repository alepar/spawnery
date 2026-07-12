package skillfetch

import (
	"context"
	"encoding/base64"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// randomishContent returns pseudo-random base64 text that gzip cannot meaningfully compress,
// so a test can size an httptest fixture against a wire cap deterministically.
func randomishContent(n int) string {
	r := rand.New(rand.NewSource(1))
	b := make([]byte, n)
	_, _ = r.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// capsClient builds a secureClient with the given caps directly (bypassing New's GitHub-only
// tarballURL) so tests can point it at an httptest server, matching the plainHTTPClient()
// technique already used in skillfetch_test.go.
func capsClient(wireCap, decompCap int64, fileCap int) *secureClient {
	return &secureClient{
		client:    &http.Client{},
		wireCap:   wireCap,
		decompCap: decompCap,
		fileCap:   fileCap,
	}
}

func TestFetcherHonoursConfiguredWireCap(t *testing.T) {
	// A tarball comfortably larger than a tiny configured wire cap.
	files := map[string]string{
		"SKILL.md": "---\nname: test-skill\n---\n# Test Skill",
		"main.md":  randomishContent(4096),
	}
	tarball := buildGzipTarball(t, "wrapper", files)
	if len(tarball) <= 256 {
		t.Fatalf("fixture too small to exceed the configured cap: %d bytes", len(tarball))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	c := capsClient(256, DefaultDecompressedCapBytes, DefaultFileCountCap)
	_, err := c.fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err == nil {
		t.Fatal("expected error: compressed tarball should exceed the configured wire cap")
	}
	if !strings.Contains(err.Error(), "wire cap") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Raising the cap above the fixture's size must succeed.
	c2 := capsClient(DefaultWireCapBytes, DefaultDecompressedCapBytes, DefaultFileCountCap)
	_, err = c2.fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err != nil {
		t.Fatalf("unexpected error with default wire cap: %v", err)
	}
}

func TestFetcherHonoursConfiguredFileCountCap(t *testing.T) {
	files := map[string]string{
		"SKILL.md": "---\nname: test-skill\n---\n# Test Skill",
		"a.md":     "a",
		"b.md":     "b",
		"c.md":     "c",
	}
	tarball := buildGzipTarball(t, "wrapper", files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	// 4 entries (SKILL.md + a.md + b.md + c.md); cap of 2 must reject.
	c := capsClient(DefaultWireCapBytes, DefaultDecompressedCapBytes, 2)
	_, err := c.fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err == nil {
		t.Fatal("expected error: file count should exceed the configured cap")
	}
	if !strings.Contains(err.Error(), "too many files") {
		t.Fatalf("unexpected error: %v", err)
	}

	c2 := capsClient(DefaultWireCapBytes, DefaultDecompressedCapBytes, DefaultFileCountCap)
	_, err = c2.fetchAndUnpack(context.Background(), srv.URL, "", "")
	if err != nil {
		t.Fatalf("unexpected error with default file count cap: %v", err)
	}
}

// TestFetcherHonoursConfiguredPlainTarCap exercises the exact check Fetch() performs after
// canonical repack (plainTarCapErr), at a configured value distinct from the default.
func TestFetcherHonoursConfiguredPlainTarCap(t *testing.T) {
	if err := plainTarCapErr(300, 256); err == nil {
		t.Fatal("expected an error: 300 bytes exceeds a 256-byte cap")
	} else if !strings.Contains(err.Error(), "exceeds size cap") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := plainTarCapErr(200, 256); err != nil {
		t.Fatalf("unexpected error: 200 bytes is within a 256-byte cap: %v", err)
	}
	// The same size that would exceed a small configured cap must pass under the default.
	if err := plainTarCapErr(300, DefaultPlainTarCapBytes); err != nil {
		t.Fatalf("unexpected error under the default cap: %v", err)
	}
}

func TestDefaultsApplyWhenConfigZero(t *testing.T) {
	f := New(Config{}).(*fetcher)
	if f.cfg.WireCapBytes != DefaultWireCapBytes {
		t.Errorf("WireCapBytes = %d, want default %d", f.cfg.WireCapBytes, DefaultWireCapBytes)
	}
	if f.cfg.DecompressedCapBytes != DefaultDecompressedCapBytes {
		t.Errorf("DecompressedCapBytes = %d, want default %d", f.cfg.DecompressedCapBytes, DefaultDecompressedCapBytes)
	}
	if f.cfg.PlainTarCapBytes != DefaultPlainTarCapBytes {
		t.Errorf("PlainTarCapBytes = %d, want default %d", f.cfg.PlainTarCapBytes, DefaultPlainTarCapBytes)
	}
	if f.cfg.FileCountCap != DefaultFileCountCap {
		t.Errorf("FileCountCap = %d, want default %d", f.cfg.FileCountCap, DefaultFileCountCap)
	}
	if f.cfg.HTTPTimeout != DefaultHTTPTimeout {
		t.Errorf("HTTPTimeout = %v, want default %v", f.cfg.HTTPTimeout, DefaultHTTPTimeout)
	}
	if f.client == nil {
		t.Fatal("expected a non-nil secureClient")
	}
}

// TestConfigCannotAddFetchOriginatingHost pins §4.6(b): the host allowlist is a code constant,
// not a config knob. A fetch always originates against api.github.com — nothing else can ever
// be the request host, regardless of what an operator puts in Config.
func TestConfigCannotAddFetchOriginatingHost(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"non-github host", "https://evil.example.com/o/r"},
		{"gitlab host", "https://gitlab.com/o/r"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRepoURL(tc.url)
			if err == nil {
				t.Fatalf("expected ParseRepoURL(%q) to fail", tc.url)
			}
			if !strings.Contains(err.Error(), "only github.com URLs are supported") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	// Config carries no host/allowlist field. Naming every field here means a reviewer adding
	// a host-selection knob to Config has to touch this test and read this comment — the real
	// invariant (origination is fixed, not configurable) is pinned by the tarballURL assertion
	// below, since Go's structural typing can't itself forbid a future field.
	_ = Config{
		GitHubToken:          "",
		ZstdLevel:            0,
		WireCapBytes:         0,
		DecompressedCapBytes: 0,
		PlainTarCapBytes:     0,
		FileCountCap:         0,
		HTTPTimeout:          0,
	}

	// tarballURL always originates against api.github.com, regardless of any config.
	for _, ref := range []struct{ owner, repo, gitRef string }{
		{"owner", "repo", ""},
		{"owner", "repo", "main"},
	} {
		u := tarballURL(ref.owner, ref.repo, ref.gitRef)
		if !strings.HasPrefix(u, "https://api.github.com/") {
			t.Fatalf("tarballURL(%v) = %q, want prefix https://api.github.com/", ref, u)
		}
	}
}
