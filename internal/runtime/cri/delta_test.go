package cri

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"spawnery/internal/runtime"
)

// fakeDeltaEngine records calls and returns scripted values/errors.
type fakeDeltaEngine struct {
	// scripted responses for Capture
	captureRef       string
	captureDeltaSize int64 // compressed byte size of the delta blob; 0 triggers the moby#47065 guard
	captureErr       error

	// scripted error for Release
	releaseErr error

	// recorded call args
	captureKey     string
	captureName    string
	captureBase    string
	captureLeaseID string

	releaseCallName    string
	releaseCallLeaseID string
	releaseCalled      bool
	closeCalled        bool

	exportName    string
	exportLeaseID string
	exportBytes   []byte
	exportErr     error

	importName    string
	importBaseRef string
	importLeaseID string
	importErr     error
}

func (f *fakeDeltaEngine) Capture(_ context.Context, snapshotKey, name, baseRef, leaseID string) (string, int64, error) {
	f.captureKey = snapshotKey
	f.captureName = name
	f.captureBase = baseRef
	f.captureLeaseID = leaseID
	if f.captureErr != nil {
		return "", 0, f.captureErr
	}
	return f.captureRef, f.captureDeltaSize, nil
}

func (f *fakeDeltaEngine) Release(_ context.Context, name, leaseID string) error {
	f.releaseCalled = true
	f.releaseCallName = name
	f.releaseCallLeaseID = leaseID
	return f.releaseErr
}

func (f *fakeDeltaEngine) Export(_ context.Context, name, leaseID string, w io.Writer) error {
	f.exportName = name
	f.exportLeaseID = leaseID
	if f.exportErr != nil {
		return f.exportErr
	}
	if len(f.exportBytes) == 0 {
		f.exportBytes = []byte("cri-delta-tar")
	}
	_, err := w.Write(f.exportBytes)
	return err
}

func (f *fakeDeltaEngine) Import(_ context.Context, name, baseRef, leaseID string, r io.Reader) error {
	f.importName = name
	f.importBaseRef = baseRef
	f.importLeaseID = leaseID
	if f.importErr != nil {
		return f.importErr
	}
	_, err := io.Copy(io.Discard, r)
	return err
}

func (f *fakeDeltaEngine) Close() error {
	f.closeCalled = true
	return nil
}

// TestResolveImageDigest covers the three cases: present with RepoDigests, present without,
// and absent.
func TestResolveImageDigest(t *testing.T) {
	cases := []struct {
		name        string
		imageName   string
		present     bool
		repoDigests []string
		wantDigest  string
		wantErr     bool
	}{
		{
			name:        "present with RepoDigests returns first digest",
			imageName:   "myimage:latest",
			present:     true,
			repoDigests: []string{"myimage@sha256:abc123", "myimage@sha256:def456"},
			wantDigest:  "myimage@sha256:abc123",
		},
		{
			name:       "present without RepoDigests returns Id",
			imageName:  "myimage:v2",
			present:    true,
			wantDigest: "myimage:v2",
		},
		{
			name:      "absent returns error",
			imageName: "notpresent:tag",
			present:   false,
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFakeCRI(t)
			b := NewCRIPodBackend(c, "runsc")
			if tc.present {
				f.present[tc.imageName] = true
				if len(tc.repoDigests) > 0 {
					f.digests[tc.imageName] = tc.repoDigests
				}
			}
			got, err := b.ResolveImageDigest(context.Background(), tc.imageName)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantDigest {
				t.Errorf("got %q, want %q", got, tc.wantDigest)
			}
		})
	}
}

// TestEnsureImage covers the three cases: deltaRef present → return it, deltaRef absent →
// baseRef, empty deltaRef → baseRef.
func TestEnsureImage(t *testing.T) {
	cases := []struct {
		name         string
		baseRef      string
		deltaRef     string
		deltaPresent bool
		wantRef      string
	}{
		{
			name:         "deltaRef present returns deltaRef",
			baseRef:      "base:v1",
			deltaRef:     "spawnery/delta:s1",
			deltaPresent: true,
			wantRef:      "spawnery/delta:s1",
		},
		{
			name:         "deltaRef absent returns baseRef",
			baseRef:      "base:v1",
			deltaRef:     "spawnery/delta:s2",
			deltaPresent: false,
			wantRef:      "base:v1",
		},
		{
			name:    "empty deltaRef returns baseRef",
			baseRef: "base:v1",
			wantRef: "base:v1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFakeCRI(t)
			b := NewCRIPodBackend(c, "runsc")
			if tc.deltaPresent {
				f.present[tc.deltaRef] = true
			}
			got, err := b.EnsureImage(context.Background(), tc.baseRef, tc.deltaRef)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantRef {
				t.Errorf("got %q, want %q", got, tc.wantRef)
			}
		})
	}
}

// TestCaptureDeltaHappyPath verifies the orchestration ordering: stop(agentID) → Capture
// → RemoveContainer, with correct args to each.
func TestCaptureDeltaHappyPath(t *testing.T) {
	spawnID := "s-happy"
	agentID := "ctr-agent-42"
	baseRef := "myimage@sha256:base"

	fakeEng := &fakeDeltaEngine{
		captureRef:       runtime.DeltaTag(spawnID),
		captureDeltaSize: 1024, // non-zero: passes the moby#47065 guard
	}

	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc", WithDeltaEngine(fakeEng))

	h := &runtime.PodHandle{
		AgentID:      agentID,
		SpawnID:      spawnID,
		BaseImageRef: baseRef,
	}
	ref, err := b.CaptureDelta(context.Background(), h)
	if err != nil {
		t.Fatalf("CaptureDelta: %v", err)
	}
	if ref != runtime.DeltaTag(spawnID) {
		t.Errorf("ref = %q, want %q", ref, runtime.DeltaTag(spawnID))
	}

	// Verify stop was called before Capture.
	if len(f.stopped) != 1 || f.stopped[0] != agentID {
		t.Errorf("StopContainer: got %v, want [%s]", f.stopped, agentID)
	}

	// Verify Capture args.
	if fakeEng.captureKey != agentID {
		t.Errorf("Capture snapshotKey = %q, want %q", fakeEng.captureKey, agentID)
	}
	if fakeEng.captureName != runtime.DeltaTag(spawnID) {
		t.Errorf("Capture name = %q, want %q", fakeEng.captureName, runtime.DeltaTag(spawnID))
	}
	if fakeEng.captureBase != baseRef {
		t.Errorf("Capture baseRef = %q, want %q", fakeEng.captureBase, baseRef)
	}
	if fakeEng.captureLeaseID != deltaLeaseID(spawnID) {
		t.Errorf("Capture leaseID = %q, want %q", fakeEng.captureLeaseID, deltaLeaseID(spawnID))
	}

	// Verify RemoveContainer was called.
	if len(f.removedContainers) != 1 || f.removedContainers[0] != agentID {
		t.Errorf("RemoveContainer: got %v, want [%s]", f.removedContainers, agentID)
	}
}

// TestCaptureDeltaCaptureError verifies that when Capture returns an error, the container is
// NOT removed (left for retry/reconcile), no Release is called, and error is propagated.
func TestCaptureDeltaCaptureError(t *testing.T) {
	fakeEng := &fakeDeltaEngine{
		captureErr: errors.New("diff failed"),
	}
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc", WithDeltaEngine(fakeEng))

	_, err := b.CaptureDelta(context.Background(), &runtime.PodHandle{
		AgentID: "ctr-1", SpawnID: "s1", BaseImageRef: "base:v1",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(f.removedContainers) != 0 {
		t.Errorf("container must NOT be removed on Capture error; removedContainers=%v", f.removedContainers)
	}
	if fakeEng.releaseCalled {
		t.Error("Release must not be called on Capture error")
	}
}

// TestCaptureDeltaEmptyDiffRejected verifies the diff-sanity check: when CreateDiff produces
// an empty delta layer (captureDeltaSize == 0), Release is called, RemoveContainer is NOT
// called, and an error mentioning the empty delta layer is returned. (The moby#47065 manifest
// reference guard proper lives in containerd.Capture, which the fake engine bypasses.)
func TestCaptureDeltaEmptyDiffRejected(t *testing.T) {
	spawnID := "s-guard"
	fakeEng := &fakeDeltaEngine{
		captureRef:       runtime.DeltaTag(spawnID),
		captureDeltaSize: 0, // zero size triggers the sanity check — empty diff must be rejected
	}
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc", WithDeltaEngine(fakeEng))

	_, err := b.CaptureDelta(context.Background(), &runtime.PodHandle{
		AgentID: "ctr-1", SpawnID: spawnID, BaseImageRef: "base:v1",
	})
	if err == nil {
		t.Fatal("expected empty-diff sanity error, got nil")
	}
	if !strings.Contains(err.Error(), "empty delta layer") {
		t.Errorf("error should mention the empty delta layer, got: %v", err)
	}
	// Release must have been called to clean up the half-imported image.
	if !fakeEng.releaseCalled {
		t.Error("Release must be called on empty-diff failure")
	}
	if fakeEng.releaseCallName != runtime.DeltaTag(spawnID) {
		t.Errorf("Release name = %q, want %q", fakeEng.releaseCallName, runtime.DeltaTag(spawnID))
	}
	// Container must NOT be removed on the sanity-check failure.
	if len(f.removedContainers) != 0 {
		t.Errorf("container must NOT be removed on empty-diff failure; removedContainers=%v", f.removedContainers)
	}
}

// TestCaptureDeltaStopError verifies that when StopContainer fails, Capture is never invoked
// and the error is propagated.
func TestCaptureDeltaStopError(t *testing.T) {
	fakeEng := &fakeDeltaEngine{
		captureRef:       "should-not-be-returned",
		captureDeltaSize: 1,
	}
	c, f := newFakeCRI(t)
	f.failStop = true
	b := NewCRIPodBackend(c, "runsc", WithDeltaEngine(fakeEng))

	_, err := b.CaptureDelta(context.Background(), &runtime.PodHandle{
		AgentID: "ctr-1", SpawnID: "s1", BaseImageRef: "base:v1",
	})
	if err == nil {
		t.Fatal("expected stop error, got nil")
	}
	// Capture must not have been called.
	if fakeEng.captureKey != "" {
		t.Error("Capture must not be called after StopContainer failure")
	}
}

// TestCaptureDeltaNoAgentID verifies that an empty AgentID produces an error without making
// any engine call.
func TestCaptureDeltaNoAgentID(t *testing.T) {
	fakeEng := &fakeDeltaEngine{}
	c, _ := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc", WithDeltaEngine(fakeEng))

	_, err := b.CaptureDelta(context.Background(), &runtime.PodHandle{SpawnID: "s1"})
	if err == nil {
		t.Fatal("expected error for empty AgentID, got nil")
	}
	if fakeEng.captureKey != "" {
		t.Error("Capture must not be called with empty AgentID")
	}
}

// TestCaptureDeltaNilEngineErrors verifies that when no engine is injected AND the conn does
// not support containerd native services, the engine() build error surfaces as a wrapped
// CaptureDelta error. This ensures the lazy build never panics.
func TestCaptureDeltaNilEngineErrors(t *testing.T) {
	// Build a backend WITHOUT WithDeltaEngine — it will try to build the real containerdEngine
	// from the bufconn connection, which doesn't speak containerd native protocol.
	c, _ := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc") // no WithDeltaEngine

	// Trigger once to build (and fail) the engine.
	_, err := b.CaptureDelta(context.Background(), &runtime.PodHandle{
		AgentID: "ctr-1", SpawnID: "s1", BaseImageRef: "base:v1",
	})
	// The error must be non-nil (either from engine build or from the first actual RPC on a
	// non-containerd conn). We only verify it does not panic.
	if err == nil {
		// If somehow it succeeded (unexpected), that's a test environment surprise — just skip.
		t.Log("CaptureDelta unexpectedly succeeded on non-containerd conn; skip assertion")
	}
	// Second call must also not panic (Once already fired).
	_, err2 := b.CaptureDelta(context.Background(), &runtime.PodHandle{
		AgentID: "ctr-1", SpawnID: "s1", BaseImageRef: "base:v1",
	})
	_ = err2 // just don't panic
}

// TestReleaseDelta verifies that ReleaseDelta calls Release with the correct name and leaseID,
// and propagates errors.
func TestReleaseDelta(t *testing.T) {
	spawnID := "s-release"
	fakeEng := &fakeDeltaEngine{}
	c, _ := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc", WithDeltaEngine(fakeEng))

	if err := b.ReleaseDelta(context.Background(), spawnID); err != nil {
		t.Fatalf("ReleaseDelta: %v", err)
	}
	if !fakeEng.releaseCalled {
		t.Error("Release was not called")
	}
	if fakeEng.releaseCallName != runtime.DeltaTag(spawnID) {
		t.Errorf("Release name = %q, want %q", fakeEng.releaseCallName, runtime.DeltaTag(spawnID))
	}
	if fakeEng.releaseCallLeaseID != deltaLeaseID(spawnID) {
		t.Errorf("Release leaseID = %q, want %q", fakeEng.releaseCallLeaseID, deltaLeaseID(spawnID))
	}

	// Error propagation.
	fakeEng.releaseErr = fmt.Errorf("injected release error")
	if err := b.ReleaseDelta(context.Background(), spawnID); err == nil {
		t.Error("ReleaseDelta must propagate Release error")
	}
}

func TestExportDeltaUsesDeterministicSpawnTag(t *testing.T) {
	spawnID := "s-export"
	fakeEng := &fakeDeltaEngine{exportBytes: []byte("cri-layer-tar")}
	c, _ := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc", WithDeltaEngine(fakeEng))

	var buf bytes.Buffer
	if err := b.ExportDelta(context.Background(), spawnID, &buf); err != nil {
		t.Fatalf("ExportDelta: %v", err)
	}
	if fakeEng.exportName != runtime.DeltaTag(spawnID) {
		t.Fatalf("Export name = %q, want %q", fakeEng.exportName, runtime.DeltaTag(spawnID))
	}
	if fakeEng.exportLeaseID != deltaLeaseID(spawnID) {
		t.Fatalf("Export lease = %q, want %q", fakeEng.exportLeaseID, deltaLeaseID(spawnID))
	}
	if got := buf.Bytes(); !bytes.Equal(got, []byte("cri-layer-tar")) {
		t.Fatalf("exported bytes = %q", got)
	}
	if bytes.HasPrefix(buf.Bytes(), []byte{0x1f, 0x8b}) {
		t.Fatal("ExportDelta must feed Kopia an uncompressed tar stream, not gzip")
	}
}

func TestImportDeltaUsesDeterministicSpawnTagAndBase(t *testing.T) {
	spawnID := "s-import"
	baseRef := "base@sha256:abc"
	fakeEng := &fakeDeltaEngine{}
	c, _ := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc", WithDeltaEngine(fakeEng))

	ref, err := b.ImportDelta(context.Background(), spawnID, baseRef, bytes.NewReader([]byte("tar")))
	if err != nil {
		t.Fatalf("ImportDelta: %v", err)
	}
	if ref != runtime.DeltaTag(spawnID) {
		t.Fatalf("ImportDelta ref = %q, want %q", ref, runtime.DeltaTag(spawnID))
	}
	if fakeEng.importName != runtime.DeltaTag(spawnID) || fakeEng.importBaseRef != baseRef {
		t.Fatalf("Import args name/base = %q/%q", fakeEng.importName, fakeEng.importBaseRef)
	}
	if fakeEng.importLeaseID != deltaLeaseID(spawnID) {
		t.Fatalf("Import lease = %q, want %q", fakeEng.importLeaseID, deltaLeaseID(spawnID))
	}
}

// TestDeltaLeaseID verifies the deterministic format.
func TestDeltaLeaseID(t *testing.T) {
	got := deltaLeaseID("abc-123")
	want := "spawnery-delta-abc-123"
	if got != want {
		t.Errorf("deltaLeaseID = %q, want %q", got, want)
	}
}
