package spawnlet

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestFetchObjectTarUsesWireCapOverStagerDefault pins sp-mwco.4.6: the CP-stamped wire cap
// (Artifact.MaxPlainTarBytes) is the effective cap, not the stager's local default — in EITHER
// direction. A wire cap smaller than the stager default must reject; a wire cap larger than the
// stager default must succeed (the regression this task exists to close: before this change,
// defaultFetcher bounded the compressed read against a hardcoded 50 MiB const regardless of any
// CP-side raise).
func TestFetchObjectTarUsesWireCapOverStagerDefault(t *testing.T) {
	const (
		stagerDefault = 1 << 10  // 1 KiB — the stager's local maxTarBytes
		wireCapSmall  = 1 << 9   // 512 B — smaller than the payload; must reject
		wireCapLarge  = 64 << 10 // 64 KiB — larger than the stager default; must accept
		payloadSize   = 16 << 10 // 16 KiB plain tar — above stagerDefault, below wireCapLarge
	)

	files := map[string]struct {
		mode os.FileMode
		body string
	}{
		"SKILL.md": {0o644, strings.Repeat("x", payloadSize)},
	}
	payload, hexSum := tarZstBytes(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	st := ArtifactStager{
		Root:        t.TempDir(),
		maxTarBytes: stagerDefault,
		fetcher: func(ctx context.Context, url string, capBytes int64) ([]byte, error) {
			return defaultFetcher(ctx, url, capBytes)
		},
	}

	// Case A: wire cap smaller than the payload -> terminal FetchError naming the small cap,
	// even though the stager's own default (1 KiB) would ALSO reject it — the point is the wire
	// cap is what's consulted and what's named in the error, not the stager default.
	smallArt := Artifact{ID: "s", PresignedURL: srv.URL, Sha256: hexSum, MaxPlainTarBytes: wireCapSmall}
	_, err := st.fetchObjectTar(context.Background(), smallArt)
	if err == nil {
		t.Fatal("expected an error: payload exceeds the small wire cap")
	}
	var fe *FetchError
	if !errors.As(err, &fe) || !fe.Terminal {
		t.Fatalf("expected a Terminal *FetchError, got %v (%T)", err, err)
	}

	// Case B (the regression case): wire cap LARGER than the stager default AND larger than the
	// payload -> must SUCCEED. Before sp-mwco.4.6, defaultFetcher bounded its compressed-body read
	// against the hardcoded maxPlainTarBytes const, so a payload/cap combination like this one
	// (raised well above any node-local default) would have failed at the fetcher regardless of
	// what the stager or the wire cap said.
	largeArt := Artifact{ID: "l", PresignedURL: srv.URL, Sha256: hexSum, MaxPlainTarBytes: wireCapLarge}
	plain, err := st.fetchObjectTar(context.Background(), largeArt)
	if err != nil {
		t.Fatalf("unexpected error with a wire cap above both the stager default and the payload: %v", err)
	}
	if len(plain) == 0 {
		t.Fatal("expected non-empty plain tar bytes")
	}
}

// TestFetchObjectTarFallsBackToStagerDefaultWhenUnset: a zero-valued Artifact.MaxPlainTarBytes
// (older CP, or a non-capped artifact) falls back to the stager's own default (tarCap()) — the
// legacy behavior is preserved, not silently disabled.
func TestFetchObjectTarFallsBackToStagerDefaultWhenUnset(t *testing.T) {
	files := map[string]struct {
		mode os.FileMode
		body string
	}{
		"SKILL.md": {0o644, strings.Repeat("x", 4096)},
	}
	payload, hexSum := tarZstBytes(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	st := ArtifactStager{
		Root:        t.TempDir(),
		maxTarBytes: 1024, // smaller than the 4 KiB payload
		fetcher: func(ctx context.Context, url string, capBytes int64) ([]byte, error) {
			return defaultFetcher(ctx, url, capBytes)
		},
	}
	art := Artifact{ID: "a", PresignedURL: srv.URL, Sha256: hexSum} // MaxPlainTarBytes unset (0)
	_, err := st.fetchObjectTar(context.Background(), art)
	if err == nil {
		t.Fatal("expected an error: payload exceeds the stager's default cap")
	}
	var fe *FetchError
	if !errors.As(err, &fe) || !fe.Terminal {
		t.Fatalf("expected a Terminal *FetchError, got %v (%T)", err, err)
	}
}

// TestDefaultFetcherBoundsCompressedBodyByCapArg calls defaultFetcher directly with an explicit
// cap argument, proving the compressed-body bound moves with the argument — not a package const.
func TestDefaultFetcherBoundsCompressedBodyByCapArg(t *testing.T) {
	body := []byte(strings.Repeat("y", 2048))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// Cap below the body size -> terminal "too large" error.
	_, err := defaultFetcher(context.Background(), srv.URL, 512)
	if err == nil {
		t.Fatal("expected an error: body exceeds the 512-byte cap")
	}
	var fe *FetchError
	if !errors.As(err, &fe) || !fe.Terminal {
		t.Fatalf("expected a Terminal *FetchError, got %v (%T)", err, err)
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Cap raised above the body size -> succeeds, proving no hardcoded const gate remains.
	got, err := defaultFetcher(context.Background(), srv.URL, 4096)
	if err != nil {
		t.Fatalf("unexpected error with a cap above the body size: %v", err)
	}
	if string(got) != string(body) {
		t.Fatal("returned body does not match the fixture")
	}
}
