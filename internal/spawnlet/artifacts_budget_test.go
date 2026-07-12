package spawnlet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// byRefFixture builds a by-ref Artifact whose PresignedURL points at path on srv, plus the raw
// zstd-compressed payload the test's handler must serve for that path.
type byRefFixture struct {
	art     Artifact
	payload []byte
}

func newByRefFixture(t *testing.T, srv *httptest.Server, id, path, body string) byRefFixture {
	t.Helper()
	payload, hexSum := tarZstBytes(t, map[string]struct {
		mode os.FileMode
		body string
	}{"f": {0o644, body}})
	return byRefFixture{
		art: Artifact{
			ID:           id,
			ContentType:  ArtifactTar,
			DestPath:     "payloads/" + id,
			PresignedURL: srv.URL + path,
			Sha256:       hexSum,
		},
		payload: payload,
	}
}

// recordingProgress collects progress() calls, and (optionally) fails the test if it is ever
// re-entered concurrently — progress must only ever be invoked from Materialize's own goroutine.
type recordingProgress struct {
	mu       sync.Mutex
	events   []string
	stepKeys []string

	entered int32 // guards re-entrancy; must be toggled 0->1->0 with no concurrent overlap
	t       *testing.T
}

func (r *recordingProgress) fn(phase, detail, stepKey string) {
	if !atomic.CompareAndSwapInt32(&r.entered, 0, 1) {
		r.t.Fatalf("progress callback re-entered concurrently: phase=%q detail=%q", phase, detail)
	}
	defer atomic.StoreInt32(&r.entered, 0)
	// Widen the window so a genuine concurrent invocation would be caught by the CAS above.
	time.Sleep(time.Millisecond)

	r.mu.Lock()
	r.events = append(r.events, detail)
	r.stepKeys = append(r.stepKeys, stepKey)
	r.mu.Unlock()
}

func (r *recordingProgress) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// --- Step 2: per-artifact progress events ---

func TestMaterialize_ProgressEventsPerByRefArtifact(t *testing.T) {
	var fixtures []byRefFixture
	var mux sync.Map // path -> payload

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

	for i := 0; i < 3; i++ {
		fx := newByRefFixture(t, srv, fmt.Sprintf("skill-%d", i), fmt.Sprintf("/obj%d", i), fmt.Sprintf("body-%d", i))
		mux.Store(fx.art.PresignedURL[len(srv.URL):], fx.payload)
		fixtures = append(fixtures, fx)
	}

	st := ArtifactStager{Root: t.TempDir(), fetcher: func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) }}
	sec := SecretInjector{Root: t.TempDir()}

	var arts []Artifact
	for _, fx := range fixtures {
		arts = append(arts, fx.art)
	}
	rec := &recordingProgress{t: t}
	if err := st.Materialize(context.Background(), "sp1", arts, sec, rec.fn); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if rec.count() < 3 {
		t.Fatalf("got %d progress events, want >= 3 (one per artifact): %v", rec.count(), rec.events)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, k := range rec.stepKeys {
		if k != "" {
			t.Fatalf("event %d stepKey = %q, want empty (staging is intra-create-pod, not a milestone)", i, k)
		}
	}
}

func TestMaterialize_HeartbeatWhileFetchInFlight(t *testing.T) {
	orig := stagingHeartbeatInterval
	stagingHeartbeatInterval = 20 * time.Millisecond
	defer func() { stagingHeartbeatInterval = orig }()

	files := map[string]struct {
		mode os.FileMode
		body string
	}{"f": {0o644, "slow"}}
	payload, hexSum := tarZstBytes(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	st := ArtifactStager{Root: t.TempDir(), fetcher: func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) }}
	sec := SecretInjector{Root: t.TempDir()}
	rec := &recordingProgress{t: t}

	art := Artifact{ID: "slow", ContentType: ArtifactTar, DestPath: "payloads/slow", PresignedURL: srv.URL + "/obj", Sha256: hexSum}
	if err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, rec.fn); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	rec.mu.Lock()
	inProgress := 0
	for _, e := range rec.events {
		if strings.Contains(e, "in progress") {
			inProgress++
		}
	}
	rec.mu.Unlock()
	if inProgress < 2 {
		t.Fatalf("got %d in-progress heartbeat events, want >= 2 (proves the stall timer would reset mid-artifact): %v", inProgress, rec.events)
	}
}

// --- Step 3: aggregate deadline ---

func TestMaterialize_AggregateDeadlineAppliesOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	sec := SecretInjector{Root: t.TempDir()}
	st := ArtifactStager{
		Root:    t.TempDir(),
		fetcher: func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) },
		budget:  100 * time.Millisecond,
	}
	var arts []Artifact
	for i := 0; i < 5; i++ {
		arts = append(arts, Artifact{
			ID: fmt.Sprintf("a%d", i), ContentType: ArtifactTar,
			DestPath: fmt.Sprintf("payloads/a%d", i), PresignedURL: srv.URL + "/obj", Sha256: "ignored",
		})
	}

	start := time.Now()
	err := st.Materialize(context.Background(), "sp1", arts, sec, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an aggregate-deadline error")
	}
	if !strings.Contains(err.Error(), "staging deadline") {
		t.Fatalf("error does not name the staging deadline: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Materialize took %v, want well under 1s (deadline must be AGGREGATE, not per-artifact x5)", elapsed)
	}
}

func TestMaterialize_ParentDeadlineStillWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	sec := SecretInjector{Root: t.TempDir()}
	// Stager budget left at its generous default; a much shorter PARENT deadline must still win —
	// no budget inflation from the (larger) stagingBudget default.
	st := ArtifactStager{
		Root:    t.TempDir(),
		fetcher: func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) },
	}
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: srv.URL + "/obj", Sha256: "ignored"}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := st.Materialize(ctx, "sp1", []Artifact{art}, sec, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a deadline error")
	}
	if elapsed > time.Second {
		t.Fatalf("Materialize took %v, want well under 1s (parent ctx deadline should win over the 5m stager default)", elapsed)
	}
}

// --- Step 4: parallel fetch ---

func TestMaterialize_ParallelFetchBounded(t *testing.T) {
	var inFlight, maxInFlight int32
	var mux sync.Map

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			cur := atomic.LoadInt32(&maxInFlight)
			if n <= cur || atomic.CompareAndSwapInt32(&maxInFlight, cur, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond) // hold the slot long enough for overlap to be observable
		atomic.AddInt32(&inFlight, -1)
		v, _ := mux.Load(r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(v.([]byte))
	}))
	defer srv.Close()

	var arts []Artifact
	for i := 0; i < 8; i++ {
		fx := newByRefFixture(t, srv, fmt.Sprintf("a%d", i), fmt.Sprintf("/obj%d", i), fmt.Sprintf("body-%d", i))
		mux.Store(fx.art.PresignedURL[len(srv.URL):], fx.payload)
		arts = append(arts, fx.art)
	}

	st := ArtifactStager{Root: t.TempDir(), fetcher: func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) }}
	sec := SecretInjector{Root: t.TempDir()}
	if err := st.Materialize(context.Background(), "sp1", arts, sec, nil); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	got := atomic.LoadInt32(&maxInFlight)
	if got < 2 || got > 4 {
		t.Fatalf("max concurrent in-flight GETs = %d, want between 2 and 4 (proves parallelism AND the stagingFetchConcurrency bound)", got)
	}
}

func TestMaterialize_TerminalFailureCancelsRest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	// Exactly stagingFetchConcurrency artifacts, one terminal, so ALL launch in the same batch (no
	// queuing) and the terminal 404 cancels peers that are genuinely in flight, not just-queued.
	var arts []Artifact
	for i := 0; i < stagingFetchConcurrency-1; i++ {
		arts = append(arts, Artifact{
			ID: fmt.Sprintf("slow%d", i), ContentType: ArtifactTar,
			DestPath: fmt.Sprintf("payloads/slow%d", i), PresignedURL: srv.URL + "/slow", Sha256: "ignored",
		})
	}
	arts = append(arts, Artifact{ID: "missing", ContentType: ArtifactTar, DestPath: "payloads/missing", PresignedURL: srv.URL + "/missing", Sha256: "ignored"})

	st := ArtifactStager{Root: t.TempDir(), fetcher: func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) }}
	sec := SecretInjector{Root: t.TempDir()}

	start := time.Now()
	err := st.Materialize(context.Background(), "sp1", arts, sec, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the terminal 404 to fail Materialize")
	}
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FetchError, got %T: %v", err, err)
	}
	if !fe.Terminal {
		t.Fatalf("404 should be Terminal, got Terminal=false")
	}
	if elapsed > time.Second {
		t.Fatalf("Materialize took %v, want well under 1s (terminal failure must cancel the other 7 in-flight 2s fetches)", elapsed)
	}
}

// --- Step 5: retry budget ---

func TestFetchWithRetry_SucceedsAfterTransientFailures(t *testing.T) {
	var gets int32
	files := map[string]struct {
		mode os.FileMode
		body string
	}{"f": {0o644, "ok"}}
	payload, hexSum := tarZstBytes(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&gets, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	st := ArtifactStager{
		Root:      t.TempDir(),
		fetcher:   func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) },
		retryBase: time.Millisecond,
	}
	sec := SecretInjector{Root: t.TempDir()}
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: srv.URL + "/obj", Sha256: hexSum}

	if err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if got := atomic.LoadInt32(&gets); got != 3 {
		t.Fatalf("GET count = %d, want exactly 3 (2 failures + 1 success)", got)
	}
}

func TestFetchWithRetry_ExhaustsAfterAttempts(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&gets, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	st := ArtifactStager{
		Root:      t.TempDir(),
		fetcher:   func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) },
		retryBase: time.Millisecond,
	}
	sec := SecretInjector{Root: t.TempDir()}
	presigned := srv.URL + "/obj?X-Amz-Signature=super-secret-query-value"
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: presigned, Sha256: "ignored"}

	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil)
	if err == nil {
		t.Fatal("expected retry exhaustion error")
	}
	if got := atomic.LoadInt32(&gets); got != retryAttempts {
		t.Fatalf("GET count = %d, want exactly %d", got, retryAttempts)
	}
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FetchError, got %T: %v", err, err)
	}
	if fe.Terminal {
		t.Fatalf("exhausted retryable failure should stay Terminal=false, got Terminal=true")
	}
	if !strings.Contains(fe.Error(), fmt.Sprintf("%d attempt", retryAttempts)) {
		t.Fatalf("exhaustion message %q does not name the attempt count", fe.Error())
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") || strings.Contains(err.Error(), "super-secret-query-value") {
		t.Fatalf("error leaks the presigned URL query: %v", err)
	}
}

func TestFetchWithRetry_TerminalShortCircuits(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&gets, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	st := ArtifactStager{
		Root:      t.TempDir(),
		fetcher:   func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) },
		retryBase: time.Millisecond,
	}
	sec := SecretInjector{Root: t.TempDir()}
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: srv.URL + "/obj", Sha256: "ignored"}

	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil)
	if err == nil {
		t.Fatal("expected 404 error")
	}
	if got := atomic.LoadInt32(&gets); got != 1 {
		t.Fatalf("GET count = %d, want exactly 1 (terminal short-circuits retry)", got)
	}
}

// --- Step 6: sha cache ---

func TestCache_HitSkipsGet(t *testing.T) {
	var gets int32
	files := map[string]struct {
		mode os.FileMode
		body string
	}{"f": {0o644, "cached-body"}}
	payload, hexSum := tarZstBytes(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&gets, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	st := ArtifactStager{
		Root:     t.TempDir(),
		CacheDir: cacheDir,
		fetcher:  func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) },
	}
	sec := SecretInjector{Root: t.TempDir()}
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: srv.URL + "/obj", Sha256: hexSum}

	if err := st.Materialize(context.Background(), "spawnA", []Artifact{art}, sec, nil); err != nil {
		t.Fatalf("Materialize spawnA: %v", err)
	}
	if err := st.Materialize(context.Background(), "spawnB", []Artifact{art}, sec, nil); err != nil {
		t.Fatalf("Materialize spawnB: %v", err)
	}

	if got := atomic.LoadInt32(&gets); got != 1 {
		t.Fatalf("GET count = %d, want exactly 1 (second Materialize should hit the cache)", got)
	}
	a, errA := os.ReadFile(filepath.Join(st.DirFor("spawnA"), "payloads", "a", "f"))
	b, errB := os.ReadFile(filepath.Join(st.DirFor("spawnB"), "payloads", "a", "f"))
	if errA != nil || errB != nil {
		t.Fatalf("read staged files: errA=%v errB=%v", errA, errB)
	}
	if string(a) != string(b) || string(a) != "cached-body" {
		t.Fatalf("staged trees differ: A=%q B=%q", a, b)
	}
}

func TestCache_CorruptEntryRefetches(t *testing.T) {
	var gets int32
	files := map[string]struct {
		mode os.FileMode
		body string
	}{"f": {0o644, "body"}}
	payload, hexSum := tarZstBytes(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&gets, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	st := ArtifactStager{
		Root:     t.TempDir(),
		CacheDir: cacheDir,
		fetcher:  func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) },
	}
	sec := SecretInjector{Root: t.TempDir()}
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: srv.URL + "/obj", Sha256: hexSum}

	if err := st.Materialize(context.Background(), "spawnA", []Artifact{art}, sec, nil); err != nil {
		t.Fatalf("Materialize spawnA: %v", err)
	}
	// Corrupt the cache entry.
	cachePath := filepath.Join(cacheDir, hexSum+".tar")
	if err := os.WriteFile(cachePath, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := st.Materialize(context.Background(), "spawnB", []Artifact{art}, sec, nil); err != nil {
		t.Fatalf("Materialize spawnB (after corruption): %v", err)
	}
	if got := atomic.LoadInt32(&gets); got != 2 {
		t.Fatalf("GET count = %d, want exactly 2 (corrupt entry must trigger a re-fetch)", got)
	}
	got, err := os.ReadFile(filepath.Join(st.DirFor("spawnB"), "payloads", "a", "f"))
	if err != nil || string(got) != "body" {
		t.Fatalf("spawnB staged content = %q err=%v, want \"body\"", got, err)
	}
}

func TestCache_EmptyShaNeverCached(t *testing.T) {
	var gets int32
	files := map[string]struct {
		mode os.FileMode
		body string
	}{"f": {0o644, "body"}}
	payload, _ := tarZstBytes(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&gets, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	st := ArtifactStager{
		Root:     t.TempDir(),
		CacheDir: t.TempDir(),
		fetcher:  func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) },
	}
	sec := SecretInjector{Root: t.TempDir()}
	// No Sha256 set: nothing to key the cache on.
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: srv.URL + "/obj"}

	if err := st.Materialize(context.Background(), "spawnA", []Artifact{art}, sec, nil); err != nil {
		t.Fatalf("Materialize spawnA: %v", err)
	}
	if err := st.Materialize(context.Background(), "spawnB", []Artifact{art}, sec, nil); err != nil {
		t.Fatalf("Materialize spawnB: %v", err)
	}
	if got := atomic.LoadInt32(&gets); got != 2 {
		t.Fatalf("GET count = %d, want exactly 2 (Sha256==\"\" must never be cached)", got)
	}
}

func TestCache_Eviction(t *testing.T) {
	origMax := cacheMaxBytes
	defer func() { cacheMaxBytes = origMax }()

	cacheDir := t.TempDir()
	st := ArtifactStager{Root: t.TempDir(), CacheDir: cacheDir}

	type fixture struct {
		art   Artifact
		plain []byte
	}
	var fixtures []fixture
	for i := 0; i < 5; i++ {
		body := strings.Repeat(fmt.Sprintf("%d", i), 100) // fixed length per entry
		plain := tarBytes(t, map[string]struct {
			mode os.FileMode
			body string
		}{"f": {0o644, body}})
		sum := sha256.Sum256(plain)
		hexSum := hex.EncodeToString(sum[:])
		fixtures = append(fixtures, fixture{art: Artifact{ID: fmt.Sprintf("a%d", i), Sha256: hexSum}, plain: plain})
	}
	// Cap sized (from an actual entry's size) to fit ~2 entries, forcing eviction of the older ones.
	cacheMaxBytes = int64(len(fixtures[0].plain))*2 + 100

	var lastSha string
	for _, fx := range fixtures {
		st.cacheStore(fx.art, fx.plain)
		lastSha = fx.art.Sha256
		// Space writes out so mtimes strictly order (evict oldest-by-mtime).
		time.Sleep(2 * time.Millisecond)
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	var newestPresent bool
	for _, e := range entries {
		info, _ := e.Info()
		total += info.Size()
		if strings.HasPrefix(e.Name(), lastSha) {
			newestPresent = true
		}
	}
	if total > cacheMaxBytes {
		t.Fatalf("cache dir size = %d, want <= cacheMaxBytes (%d)", total, cacheMaxBytes)
	}
	if !newestPresent {
		t.Fatalf("newest cache entry %s was evicted; eviction should prefer removing the oldest", lastSha)
	}
}

// --- Step 9: bundle-scale acceptance ---

func TestMaterialize_BundleScaleAcceptance(t *testing.T) {
	const n = 20
	var mux sync.Map

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		v, ok := mux.Load(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(v.([]byte))
	}))
	defer srv.Close()

	var arts []Artifact
	for i := 0; i < n; i++ {
		fx := newByRefFixture(t, srv, fmt.Sprintf("skill-%d", i), fmt.Sprintf("/obj%d", i), fmt.Sprintf("payload-%d", i))
		mux.Store(fx.art.PresignedURL[len(srv.URL):], fx.payload)
		arts = append(arts, fx.art)
	}

	st := ArtifactStager{Root: t.TempDir(), fetcher: func(ctx context.Context, url string) ([]byte, error) { return defaultFetcher(ctx, url) }}
	sec := SecretInjector{Root: t.TempDir()}
	rec := &recordingProgress{t: t}

	var mu sync.Mutex
	var eventTimes []time.Time
	wrapped := func(phase, detail, stepKey string) {
		rec.fn(phase, detail, stepKey)
		mu.Lock()
		eventTimes = append(eventTimes, time.Now())
		mu.Unlock()
	}

	start := time.Now()
	if err := st.Materialize(context.Background(), "sp-bundle", arts, sec, wrapped); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// (a) succeeded (checked via err above); (c) all 20 trees staged.
	for i := 0; i < n; i++ {
		p := filepath.Join(st.DirFor("sp-bundle"), "payloads", fmt.Sprintf("skill-%d", i), "f")
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("artifact %d not staged: %v", i, err)
		}
	}

	// (b) progress keeps coming from the very start of staging — 20 artifacts at
	// stagingFetchConcurrency=4 means dispatch itself takes multiple ~50ms rounds; if the dispatch
	// loop (semaphore-gated) ran ahead of the progress collector instead of concurrently with it, the
	// FIRST progress event would arrive only after most of the bundle had already finished, silently
	// reopening the stall-window gap this task exists to close. Check the gap from Materialize's own
	// start, not just gaps between recorded events (which stay small even if they all land late).
	if len(eventTimes) < n {
		t.Fatalf("got %d progress events, want >= %d", len(eventTimes), n)
	}
	if gap := eventTimes[0].Sub(start); gap > time.Second {
		t.Fatalf("first progress event arrived %v after Materialize started, want < 1s (dispatch must run concurrently with progress collection, not before it)", gap)
	}
	var maxGap time.Duration
	prev := start
	for _, et := range eventTimes {
		if gap := et.Sub(prev); gap > maxGap {
			maxGap = gap
		}
		prev = et
	}
	if maxGap > time.Second {
		t.Fatalf("longest gap between progress events (incl. from start) = %v, want < 1s", maxGap)
	}
}
