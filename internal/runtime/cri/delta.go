package cri

import (
	"context"
	"fmt"
	"io"

	"spawnery/internal/runtime"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// deltaEngine is the seam between tested orchestration and raw containerd calls (e2e only).
// All types crossing the seam are strings so the fake is trivial to implement in tests.
type deltaEngine interface {
	// Capture diffs the rw snapshot keyed by snapshotKey (CRI container id, k8s.io ns) against its
	// parent chain, assembles per-spawn image `name` = baseRef layers + the delta layer, pinned by
	// lease leaseID, and returns the image ref and the byte size of the produced delta layer.
	// A zero/negative size indicates the diff produced no bytes and is treated as a guard failure.
	// On any error it leaves no half-imported image (internal best-effort cleanup of the lease/blobs).
	Capture(ctx context.Context, snapshotKey, name, baseRef, leaseID string) (ref string, deltaSize int64, err error)
	// Release drops the per-spawn image record and its pinning lease (GC hook).
	Release(ctx context.Context, name, leaseID string) error
	// ExportTopLayer streams ONLY the per-spawn image's top (delta) layer blob — not the whole
	// image. Delta-only migration: the base stays resident on both nodes (sp-ei4.1.14).
	ExportTopLayer(ctx context.Context, name string, w io.Writer) error
	// AssembleOnBase writes the shipped delta layer blob and reconstructs newTag = baseRef's
	// layers + that delta, pinned with leaseID. baseRef must already be present on the target.
	AssembleOnBase(ctx context.Context, baseRef, newTag, leaseID string, r io.Reader) error
	// Pause/Resume the agent container's containerd task (suspend quiescence, spec §3).
	// key is the CRI container id (k8s.io namespace).
	Pause(ctx context.Context, key string) error
	Resume(ctx context.Context, key string) error
	// Close releases any resources held by the engine. Does not close the shared gRPC conn.
	Close() error
}

// Option configures a CRIPodBackend.
type Option func(*CRIPodBackend)

// WithDeltaEngine injects a deltaEngine, replacing the lazily-built containerdEngine.
// Used in unit tests to avoid requiring a real containerd daemon.
func WithDeltaEngine(e deltaEngine) Option {
	return func(b *CRIPodBackend) { b.delta = e }
}

// deltaLeaseID returns the deterministic per-spawn lease name that pins delta blobs.
// Both CaptureDelta and ReleaseDelta derive it from the spawnID to keep names consistent.
func deltaLeaseID(spawnID string) string { return "spawnery-delta-" + spawnID }

// engine returns the deltaEngine for this backend, building it lazily on first use.
// Builds the real containerdEngine from the shared CRI gRPC connection. If opts injected
// a fake engine (WithDeltaEngine), that is returned without building the real one.
// All reads of b.delta are routed through the Once to prevent a data race: a concurrent
// first-call to engine() on a nil b.delta (no injection) would race with the write inside
// Do without this synchronization.
func (b *CRIPodBackend) engine() (deltaEngine, error) {
	b.deltaOnce.Do(func() {
		if b.delta == nil {
			b.delta, b.deltaErr = newContainerdEngine(b.c.conn)
		}
		// else: already set by WithDeltaEngine (tests); nothing to do.
	})
	return b.delta, b.deltaErr
}

// ResolveImageDigest returns the content-addressable digest of ref via the CRI ImageService:
// RepoDigests[0] when present, fallback to Image.Id. Mirrors the docker-lane semantics.
func (b *CRIPodBackend) ResolveImageDigest(ctx context.Context, ref string) (string, error) {
	st, err := b.c.image.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{Image: &runtimeapi.ImageSpec{Image: ref}})
	if err != nil {
		return "", fmt.Errorf("cri image status %q: %w", ref, err)
	}
	img := st.GetImage()
	if img == nil {
		return "", fmt.Errorf("image %q not found", ref)
	}
	if len(img.RepoDigests) > 0 {
		return img.RepoDigests[0], nil
	}
	return img.Id, nil
}

// EnsureImage returns the image ref to launch the agent from. If deltaRef is non-empty and
// present in the CRI image store it is returned (resume from delta); otherwise baseRef is
// returned. Uses the CRI image store — the same store the runtime launches containers from.
func (b *CRIPodBackend) EnsureImage(ctx context.Context, baseRef, deltaRef string) (string, error) {
	if deltaRef != "" {
		st, err := b.c.image.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{Image: &runtimeapi.ImageSpec{Image: deltaRef}})
		if err == nil && st.GetImage() != nil {
			return deltaRef, nil
		}
	}
	return baseRef, nil
}

// CaptureDelta stops the agent container, diffs its snapshot via containerd DiffService, assembles
// a per-spawn image (base layers + delta layer, lease-pinned) — assembly asserts the manifest
// references the delta descriptor (the moby#47065 reference guard lives in containerd.Capture) —
// then sanity-checks the diff produced a non-empty layer and removes the container. Returns the
// assembled image ref ("spawnery/delta:<spawnID>").
func (b *CRIPodBackend) CaptureDelta(ctx context.Context, h *runtime.PodHandle) (string, error) {
	return b.CaptureDeltaAs(ctx, h, h.SpawnID)
}

func (b *CRIPodBackend) CaptureDeltaAs(ctx context.Context, h *runtime.PodHandle, targetSpawnID string) (string, error) {
	if targetSpawnID != h.SpawnID {
		return "", fmt.Errorf("cri source-preserving fork capture from %s as %s is unsupported", h.SpawnID, targetSpawnID)
	}
	eng, err := b.engine()
	if err != nil {
		return "", fmt.Errorf("cri delta engine: %w", err)
	}
	if h.AgentID == "" {
		return "", fmt.Errorf("cri capture: no agent container id")
	}

	name := runtime.DeltaTag(targetSpawnID)
	leaseID := deltaLeaseID(targetSpawnID)

	// Stop the container (not remove) so its snapshot is quiesced before diff.
	if _, err := b.c.runtime.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: h.AgentID}); err != nil {
		return "", fmt.Errorf("cri capture stop %s: %w", h.AgentID, err)
	}

	ref, deltaSize, err := eng.Capture(ctx, h.AgentID, name, h.BaseImageRef, leaseID)
	if err != nil {
		return "", fmt.Errorf("cri capture %s as %s: %w", h.SpawnID, targetSpawnID, err)
	}

	// Diff-sanity check (distinct from the manifest reference guard in containerd.Capture):
	// a zero/negative size means CreateDiff silently returned an empty/corrupt result, which
	// would pin a degenerate delta layer. Reject it so the next resume falls back to the base.
	if deltaSize <= 0 {
		_ = eng.Release(context.WithoutCancel(ctx), name, leaseID)
		return "", fmt.Errorf("cri capture %s: diff produced empty delta layer (size=%d)",
			targetSpawnID, deltaSize)
	}

	// Best-effort remove after capture is pinned. Mirrors the docker lane's best-effort Stop.
	// The subsequent Manager Stop→removeSandbox reaps any leftover if this fails.
	_, _ = b.c.runtime.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: h.AgentID})

	return ref, nil
}

// ReleaseDelta drops the per-spawn delta image and its pinning lease (GC).
func (b *CRIPodBackend) ReleaseDelta(ctx context.Context, spawnID string) error {
	eng, err := b.engine()
	if err != nil {
		return fmt.Errorf("cri delta engine: %w", err)
	}
	return eng.Release(ctx, runtime.DeltaTag(spawnID), deltaLeaseID(spawnID))
}

func (b *CRIPodBackend) ExportDelta(ctx context.Context, spawnID string, w io.Writer) error {
	eng, err := b.engine()
	if err != nil {
		return fmt.Errorf("cri delta engine: %w", err)
	}
	name := runtime.DeltaTag(spawnID)
	if err := eng.ExportTopLayer(ctx, name, w); err != nil {
		return fmt.Errorf("cri export delta %s: %w", name, err)
	}
	return nil
}

// Pause pauses the AGENT container's containerd task (quiesces agent writes before the final
// snapshot, spec §3). Empty AgentID is a caller bug — returns an error without building the engine.
func (b *CRIPodBackend) Pause(ctx context.Context, h *runtime.PodHandle) error {
	if h.AgentID == "" {
		return fmt.Errorf("cri pause: no agent container id")
	}
	eng, err := b.engine()
	if err != nil {
		return fmt.Errorf("cri delta engine: %w", err)
	}
	if err := eng.Pause(ctx, h.AgentID); err != nil {
		return fmt.Errorf("cri pause %s: %w", h.AgentID, err)
	}
	return nil
}

// Unpause resumes a previously-paused agent container's containerd task.
func (b *CRIPodBackend) Unpause(ctx context.Context, h *runtime.PodHandle) error {
	if h.AgentID == "" {
		return fmt.Errorf("cri unpause: no agent container id")
	}
	eng, err := b.engine()
	if err != nil {
		return fmt.Errorf("cri delta engine: %w", err)
	}
	if err := eng.Resume(ctx, h.AgentID); err != nil {
		return fmt.Errorf("cri unpause %s: %w", h.AgentID, err)
	}
	return nil
}

func (b *CRIPodBackend) ImportDelta(ctx context.Context, spawnID, baseRef string, r io.Reader) (string, error) {
	eng, err := b.engine()
	if err != nil {
		return "", fmt.Errorf("cri delta engine: %w", err)
	}
	name := runtime.DeltaTag(spawnID)
	if err := eng.AssembleOnBase(ctx, baseRef, name, deltaLeaseID(spawnID), r); err != nil {
		return "", fmt.Errorf("cri import delta %s: %w", name, err)
	}
	return name, nil
}
