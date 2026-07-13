package spawnlet

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// --- sp-mwco.4.5: resume gating was spike-answered KILLED (never implemented). These tests guard
// the invariants naive gating would have broken, per
// docs/superpowers/specs/2026-07-12-skill-delivery-hardening-design.md §4.5. ---

// assertResumeStagingIntact fails the test unless spawnID's staging dir holds the inline
// manifest.json AND every by-ref payload tree named in wantPayloads (id -> expected file body).
// This is exactly the contract a gated resume would violate: agentinstall reads manifest.json and
// expects every payload dir it names to already exist on disk ("skill source directory does not
// exist" is the StatusFailed naive gating would produce for every skill, on every resume).
func assertResumeStagingIntact(t *testing.T, st ArtifactStager, spawnID string, wantPayloads map[string]string) {
	t.Helper()
	dir := st.DirFor(spawnID)
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("manifest.json missing from staging dir: %v", err)
	}
	for id, wantBody := range wantPayloads {
		p := filepath.Join(dir, "payloads", id, "f")
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("payload dir for %q missing (this is the naive-gating failure: agentinstall would "+
				"report 'skill source directory does not exist'): %v", id, err)
		}
		if string(got) != wantBody {
			t.Fatalf("payload for %q = %q, want %q", id, got, wantBody)
		}
	}
}

// TestMaterialize_ResumeReStagesEveryByRefArtifact is the anti-gating guard (sp-mwco.4.5 §3.1): a
// same-spawn-ID resume re-thread must land the inline manifest.json artifact AND every by-ref
// payload tree again, exactly as the first create did. Materialize wipes and recreates the
// per-spawn staging dir unconditionally on every call (artifacts.go:343), so skipping the by-ref
// re-fetch "because it's a resume" would leave manifest.json naming payload dirs that were never
// re-populated -- the exact failure mode the spike killed resume gating over.
//
// Teeth check (recorded per the plan, not committed): patching Materialize to early-return when
// DirFor(spawnID) already exists before the RemoveAll/MkdirAll (the naive gate) makes this test
// fail on the second Materialize call with "payload dir for ... missing" for both skills, exactly
// as predicted. Reverted before committing.
func TestMaterialize_ResumeReStagesEveryByRefArtifact(t *testing.T) {
	var mux sync.Map // URL path -> zstd tar payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := mux.Load(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(v.([]byte))
	}))
	defer srv.Close()

	fxA := newByRefFixture(t, srv, "skill-a", "/obj-a", "body-a")
	fxB := newByRefFixture(t, srv, "skill-b", "/obj-b", "body-b")
	mux.Store(fxA.art.PresignedURL[len(srv.URL):], fxA.payload)
	mux.Store(fxB.art.PresignedURL[len(srv.URL):], fxB.payload)

	manifest := Artifact{
		ID:          "manifest",
		Inline:      []byte(`{"skills":["skill-a","skill-b"]}`),
		ContentType: ArtifactBytes,
		DestPath:    "manifest.json",
		Mode:        0o644,
	}
	specs := []Artifact{manifest, fxA.art, fxB.art}
	want := map[string]string{"skill-a": "body-a", "skill-b": "body-b"}

	st := ArtifactStager{Root: t.TempDir(), fetcher: func(ctx context.Context, url string, capBytes int64) ([]byte, error) {
		return defaultFetcher(ctx, url, capBytes)
	}}
	sec := SecretInjector{Root: t.TempDir()}

	// First call: the create path.
	if err := st.Materialize(context.Background(), "sp1", specs, sec, nil, nil); err != nil {
		t.Fatalf("create Materialize: %v", err)
	}
	assertResumeStagingIntact(t, st, "sp1", want)

	// Second call, same spawn ID, same specs: the resume re-thread (manager.go:1261 calls
	// Materialize identically on create and resume). Naive gating would skip re-fetching the
	// by-ref artifacts here; assert it did not.
	if err := st.Materialize(context.Background(), "sp1", specs, sec, nil, nil); err != nil {
		t.Fatalf("resume Materialize: %v", err)
	}
	assertResumeStagingIntact(t, st, "sp1", want)
}

// TestMaterialize_ResumeServedFromShaCacheWhenFetchFails demonstrates the property that actually
// makes a same-node resume survive a Garage outage: not resume gating (killed), but §4.1's
// node-local sha-keyed cache. The first Materialize (create) populates the cache via a working
// fetcher; the second Materialize (resume re-thread, same spawn ID) uses a fetcher that always
// fails as if Garage were down, and must still succeed by serving the by-ref artifact from cache
// without ever invoking the failing fetcher.
func TestMaterialize_ResumeServedFromShaCacheWhenFetchFails(t *testing.T) {
	files := map[string]struct {
		mode os.FileMode
		body string
	}{"f": {0o644, "cached-payload"}}
	payload, hexSum := tarZstBytes(t, files)

	root := t.TempDir()
	cacheDir := t.TempDir()
	sec := SecretInjector{Root: t.TempDir()}
	art := Artifact{
		ID:           "skill",
		ContentType:  ArtifactTar,
		DestPath:     "payloads/skill",
		PresignedURL: "https://example.invalid/obj",
		Sha256:       hexSum,
	}

	// Initial create: a working fetcher populates the node-local cache.
	warm := ArtifactStager{
		Root:     root,
		CacheDir: cacheDir,
		fetcher: func(ctx context.Context, url string, capBytes int64) ([]byte, error) {
			return payload, nil
		},
	}
	if err := warm.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, nil); err != nil {
		t.Fatalf("initial (create) Materialize: %v", err)
	}

	// Resume re-thread, same spawn ID, same CacheDir: Garage is down, every GET fails. The warm
	// cache must serve the payload without the fetcher ever being called.
	var gets int32
	cold := ArtifactStager{
		Root:     root,
		CacheDir: cacheDir,
		fetcher: func(ctx context.Context, url string, capBytes int64) ([]byte, error) {
			atomic.AddInt32(&gets, 1)
			return nil, errors.New("dial tcp: connection refused (Garage down)")
		},
	}
	if err := cold.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, nil); err != nil {
		t.Fatalf("resume Materialize with Garage down: %v", err)
	}
	if got := atomic.LoadInt32(&gets); got != 0 {
		t.Fatalf("failing fetcher invoked %d time(s) on resume, want 0 (cache hit must skip the GET entirely)", got)
	}
	got, err := os.ReadFile(filepath.Join(cold.DirFor("sp1"), "payloads", "skill", "f"))
	if err != nil || string(got) != "cached-payload" {
		t.Fatalf("resumed payload = %q err=%v, want %q", got, err, "cached-payload")
	}
}
