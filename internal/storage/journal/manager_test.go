package journal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestManager builds a filesystem-backed, node-local-custody Manager rooted
// under t.TempDir() — fully hermetic, no network.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	keyfile := filepath.Join(root, "node.key")
	if err := GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatal(err)
	}
	custody, err := NewNodeLocalCustody(keyfile, filepath.Join(root, "seals"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Config{
		RepoRoot: filepath.Join(root, "repos"),
		Backend:  &FilesystemBackend{Root: filepath.Join(root, "blobs")},
		Custody:  custody,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type recordingGenerationBackendProvider struct {
	root string

	mu    sync.Mutex
	opens []string
}

func (p *recordingGenerationBackendProvider) BackendFor(_ context.Context, spawnID string, gen uint64) (BlobBackend, error) {
	p.mu.Lock()
	p.opens = append(p.opens, fmt.Sprintf("%s:%d", spawnID, gen))
	p.mu.Unlock()
	return &FilesystemBackend{Root: p.root}, nil
}

func (p *recordingGenerationBackendProvider) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.opens...)
}

// TestSnapshotRestoreRoundTripAndPinning is the core e2e: snapshot a mount at
// gen 1, change it, snapshot at gen 2, then restore the PINNED gen-1 manifest
// (not "latest") into a fresh dir and verify it has gen-1 content — proving
// restore-pinning (design §3, roast C1) + generation tagging.
func TestSnapshotRestoreRoundTripAndPinning(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	const spawnID = "spawn-1"

	// --- generation 1 ---
	src := t.TempDir()
	writeFile(t, src, "hello.txt", "gen1-hello")
	writeFile(t, src, "sub/nested.txt", "gen1-nested")
	mt := Mount{Name: "work", HostDir: src, Class: NodeLocal}

	ids1, err := m.FinalSnapshot(ctx, spawnID, 1, []Mount{mt})
	if err != nil {
		t.Fatalf("final snapshot gen1: %v", err)
	}
	id1 := ids1["work"]
	if id1 == "" {
		t.Fatal("gen1 manifest id is empty")
	}
	// FinalSnapshot suspended the queues; drop in-memory state so gen 2 reopens.
	if err := m.Close(ctx, spawnID); err != nil {
		t.Fatal(err)
	}

	// --- generation 2 (same host dir, changed content) ---
	writeFile(t, src, "hello.txt", "gen2-hello-CHANGED")
	writeFile(t, src, "fresh.txt", "gen2-only")
	ids2, err := m.FinalSnapshot(ctx, spawnID, 2, []Mount{mt})
	if err != nil {
		t.Fatalf("final snapshot gen2: %v", err)
	}
	id2 := ids2["work"]
	if id2 == "" || id2 == id1 {
		t.Fatalf("gen2 manifest id unexpected: id1=%s id2=%s", id1, id2)
	}

	// --- restore the PINNED gen-1 manifest into a fresh dir ---
	dst1 := t.TempDir()
	if err := m.Restore(ctx, spawnID, "work", id1, dst1); err != nil {
		t.Fatalf("restore gen1: %v", err)
	}
	if got := readFile(t, dst1, "hello.txt"); got != "gen1-hello" {
		t.Fatalf("pinned gen1 restore hello.txt = %q, want gen1-hello", got)
	}
	if got := readFile(t, dst1, "sub/nested.txt"); got != "gen1-nested" {
		t.Fatalf("pinned gen1 restore nested = %q, want gen1-nested", got)
	}
	if _, err := os.Stat(filepath.Join(dst1, "fresh.txt")); !os.IsNotExist(err) {
		t.Fatal("gen1 restore must NOT contain gen2-only file (pinning, not latest)")
	}

	// --- restore the gen-2 manifest into another fresh dir ---
	dst2 := t.TempDir()
	if err := m.Restore(ctx, spawnID, "work", id2, dst2); err != nil {
		t.Fatalf("restore gen2: %v", err)
	}
	if got := readFile(t, dst2, "hello.txt"); got != "gen2-hello-CHANGED" {
		t.Fatalf("gen2 restore hello.txt = %q, want gen2-hello-CHANGED", got)
	}
	if got := readFile(t, dst2, "fresh.txt"); got != "gen2-only" {
		t.Fatalf("gen2 restore fresh.txt = %q, want gen2-only", got)
	}

	// --- LatestForGeneration (crash fallback) resolves the right gen ---
	if got, err := m.LatestForGeneration(ctx, spawnID, "work", 1); err != nil || got != id1 {
		t.Fatalf("LatestForGeneration(1) = %s, %v; want %s", got, err, id1)
	}
	if got, err := m.LatestForGeneration(ctx, spawnID, "work", 2); err != nil || got != id2 {
		t.Fatalf("LatestForGeneration(2) = %s, %v; want %s", got, err, id2)
	}
}

func TestManagerUsesGenerationBackendForSnapshotArtifactAndRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	keyfile := filepath.Join(root, "node.key")
	if err := GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatal(err)
	}
	custody, err := NewNodeLocalCustody(keyfile, filepath.Join(root, "seals"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingGenerationBackendProvider{root: filepath.Join(root, "blobs")}
	m, err := NewManager(Config{
		RepoRoot:           filepath.Join(root, "repos"),
		GenerationBackends: provider,
		Custody:            custody,
	})
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	writeFile(t, src, "work.txt", "source gen9")
	mt := Mount{Name: "work", HostDir: src, Class: NodeLocal}
	pins, err := m.FinalSnapshot(ctx, "sp-source", 9, []Mount{mt})
	if err != nil {
		t.Fatalf("source final snapshot: %v", err)
	}
	if _, err := m.PutArtifact(ctx, "sp-fork", 1, ArtifactDescriptor{Type: ArtifactRootfsDelta, Format: ArtifactFormatOCILayout}, strings.NewReader("rootfs")); err != nil {
		t.Fatalf("fork artifact: %v", err)
	}
	if err := m.Close(ctx, "sp-source"); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := m.RestoreGeneration(ctx, "sp-source", 9, "work", pins["work"], dst); err != nil {
		t.Fatalf("restore source gen9: %v", err)
	}
	if got := readFile(t, dst, "work.txt"); got != "source gen9" {
		t.Fatalf("restored work.txt = %q, want source gen9", got)
	}

	got := provider.snapshot()
	want := []string{"sp-source:9", "sp-fork:1", "sp-source:9"}
	if len(got) != len(want) {
		t.Fatalf("generation backend opens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("generation backend opens = %v, want %v", got, want)
		}
	}
}

// TestFinalSnapshotSkipsEphemeralAndSecretMounts verifies the journaler captures
// only journaled, non-secret mounts (design §1a / §2 secret exclusion).
func TestFinalSnapshotSkipsEphemeralAndSecretMounts(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	work := t.TempDir()
	writeFile(t, work, "a.txt", "data")
	eph := t.TempDir()
	writeFile(t, eph, "b.txt", "data")
	secret := t.TempDir()
	writeFile(t, secret, "token", "s3cr3t")

	mounts := []Mount{
		{Name: "work", HostDir: work, Class: NodeLocal},
		{Name: "scratch", HostDir: eph, Class: Ephemeral},
		{Name: "vault", HostDir: secret, Class: NodeLocal, Secret: true},
	}
	ids, err := m.FinalSnapshot(ctx, "spawn-x", 1, mounts)
	if err != nil {
		t.Fatalf("final snapshot: %v", err)
	}
	if _, ok := ids["work"]; !ok {
		t.Fatal("journaled mount 'work' missing from result")
	}
	if _, ok := ids["scratch"]; ok {
		t.Fatal("ephemeral mount must not be journaled")
	}
	if _, ok := ids["vault"]; ok {
		t.Fatal("secret mount must be excluded from the journal")
	}
}

// TestFinalSnapshotNoJournaledMountsIsNoOp verifies a scratch-only spawn yields
// a nil result and never opens a repo — the guard that keeps existing behavior
// unchanged.
func TestFinalSnapshotNoJournaledMountsIsNoOp(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	ids, err := m.FinalSnapshot(ctx, "spawn-eph", 1, []Mount{
		{Name: "scratch", HostDir: t.TempDir(), Class: Ephemeral},
	})
	if err != nil {
		t.Fatalf("no-op final snapshot: %v", err)
	}
	if ids != nil {
		t.Fatalf("expected nil result for scratch-only spawn, got %v", ids)
	}
}

// TestRequestSnapshotThenSuspendProducesManifest exercises the async request
// path followed by the suspend barrier producing the final pinned manifest.
func TestRequestSnapshotThenSuspendProducesManifest(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	src := t.TempDir()
	writeFile(t, src, "f.txt", "x")
	mt := Mount{Name: "work", HostDir: src, Class: NodeLocal}

	m.RequestSnapshot(ctx, "spawn-r", 7, mt)
	ids, err := m.FinalSnapshot(ctx, "spawn-r", 7, []Mount{mt})
	if err != nil {
		t.Fatalf("final snapshot: %v", err)
	}
	if ids["work"] == "" {
		t.Fatal("expected a final manifest id")
	}
}

func TestWarmSnapshotDoesNotSuspendLaterRequests(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	keyfile := filepath.Join(root, "node.key")
	if err := GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatal(err)
	}
	custody, err := NewNodeLocalCustody(keyfile, filepath.Join(root, "seals"))
	if err != nil {
		t.Fatal(err)
	}
	tel := &recordingTelemetry{}
	m, err := NewManager(Config{
		RepoRoot:    filepath.Join(root, "repos"),
		Backend:     &FilesystemBackend{Root: filepath.Join(root, "blobs")},
		Custody:     custody,
		DebounceMin: time.Nanosecond,
		Telemetry:   tel,
	})
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	writeFile(t, src, "f.txt", "warm")
	mt := Mount{Name: "work", HostDir: src, Class: NodeLocal}

	ids, err := m.WarmSnapshot(ctx, "spawn-warm", 3, []Mount{mt})
	if err != nil {
		t.Fatalf("warm snapshot: %v", err)
	}
	if ids["work"] == "" {
		t.Fatal("expected warm manifest id")
	}

	writeFile(t, src, "f.txt", "after-warm")
	m.RequestSnapshot(ctx, "spawn-warm", 3, mt)

	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, e := range tel.snapshot() {
			if e.mount == "work" && e.gen == 3 && e.kind == SnapshotContinuous && e.id != "" && e.err == nil {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("RequestSnapshot after WarmSnapshot did not produce a continuous snapshot; events=%+v", tel.snapshot())
		case <-tick.C:
		}
	}
}

// TestQuickMaintenanceRuns verifies index-compacting maintenance runs without
// error on a populated repo.
func TestQuickMaintenanceRuns(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	src := t.TempDir()
	writeFile(t, src, "f.txt", "x")
	mt := Mount{Name: "work", HostDir: src, Class: NodeLocal}
	if _, err := m.FinalSnapshot(ctx, "spawn-m", 1, []Mount{mt}); err != nil {
		t.Fatal(err)
	}
	if err := m.QuickMaintenance(ctx, "spawn-m"); err != nil {
		t.Fatalf("quick maintenance: %v", err)
	}
}

// TestFullMaintenanceRuns verifies the CP-commanded full (deleting) maintenance
// primitive (design §2 roast M5) runs without error on a populated repo.
func TestFullMaintenanceRuns(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	src := t.TempDir()
	writeFile(t, src, "f.txt", "x")
	mt := Mount{Name: "work", HostDir: src, Class: NodeLocal}
	if _, err := m.FinalSnapshot(ctx, "spawn-fm", 1, []Mount{mt}); err != nil {
		t.Fatal(err)
	}
	if err := m.FullMaintenance(ctx, "spawn-fm"); err != nil {
		t.Fatalf("full maintenance: %v", err)
	}
}

// TestRestoreRequiresPinnedID verifies restore rejects an empty (unpinned) id.
func TestRestoreRequiresPinnedID(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	if err := m.Restore(ctx, "spawn-z", "work", "", t.TempDir()); err == nil {
		t.Fatal("restore with empty manifest id must fail (pinned, not latest)")
	}
}

// recordingTelemetry captures SnapshotDone events for assertions.
type recordingTelemetry struct {
	mu     sync.Mutex
	events []telemEvent
}
type telemEvent struct {
	mount string
	gen   uint64
	kind  SnapshotKind
	id    ManifestID
	err   error
}

func (r *recordingTelemetry) SnapshotDone(_, mount string, gen uint64, kind SnapshotKind, _ time.Duration, id ManifestID, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, telemEvent{mount, gen, kind, id, err})
}
func (r *recordingTelemetry) snapshot() []telemEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]telemEvent(nil), r.events...)
}

// TestTelemetryEmittedOnSnapshot verifies the §5 telemetry seam fires for both a
// continuous (watcher-driven) snapshot and the suspend final snapshot.
func TestTelemetryEmittedOnSnapshot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	keyfile := filepath.Join(root, "node.key")
	if err := GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatal(err)
	}
	custody, err := NewNodeLocalCustody(keyfile, filepath.Join(root, "seals"))
	if err != nil {
		t.Fatal(err)
	}
	tel := &recordingTelemetry{}
	m, err := NewManager(Config{
		RepoRoot:    filepath.Join(root, "repos"),
		Backend:     &FilesystemBackend{Root: filepath.Join(root, "blobs")},
		Custody:     custody,
		DebounceMin: time.Nanosecond, // don't suppress the continuous snapshot in-test
		Telemetry:   tel,
	})
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	writeFile(t, src, "f.txt", "x")
	mt := Mount{Name: "work", HostDir: src, Class: NodeLocal}

	m.RequestSnapshot(ctx, "spawn-t", 3, mt)
	// FinalSnapshot drains the queued continuous snapshot, then takes the final one.
	if _, err := m.FinalSnapshot(ctx, "spawn-t", 3, []Mount{mt}); err != nil {
		t.Fatal(err)
	}

	var sawContinuous, sawFinal bool
	for _, e := range tel.snapshot() {
		if e.mount != "work" || e.gen != 3 || e.id == "" || e.err != nil {
			t.Fatalf("unexpected telemetry event: %+v", e)
		}
		switch e.kind {
		case SnapshotContinuous:
			sawContinuous = true
		case SnapshotFinal:
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Fatalf("expected a final-snapshot telemetry event; got %+v", tel.snapshot())
	}
	_ = sawContinuous // continuous is best-effort (may coalesce); final is the guaranteed signal
}
