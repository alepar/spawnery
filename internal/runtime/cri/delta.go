package cri

import (
	"context"
	"fmt"
	"slices"

	"spawnery/internal/runtime"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// deltaEngine is the seam between tested orchestration and raw containerd calls (e2e only).
// All types crossing the seam are strings so the fake is trivial to implement in tests.
type deltaEngine interface {
	// Capture diffs the rw snapshot keyed by snapshotKey (CRI container id, k8s.io ns) against its
	// parent chain, assembles per-spawn image `name` = baseRef layers + the delta layer, pinned by
	// lease leaseID, and returns the image ref, the delta layer digest, and ALL layer digests the
	// assembled manifest references (for the moby#47065 guard). On any error it leaves no
	// half-imported image (internal best-effort cleanup of the lease/blobs).
	Capture(ctx context.Context, snapshotKey, name, baseRef, leaseID string) (ref, deltaDigest string, manifestLayers []string, err error)
	// Release drops the per-spawn image record and its pinning lease (GC hook).
	Release(ctx context.Context, name, leaseID string) error
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
// a fake engine (WithDeltaEngine), that is returned immediately without building the real one.
func (b *CRIPodBackend) engine() (deltaEngine, error) {
	if b.delta != nil {
		return b.delta, nil // injected (tests) or already built
	}
	b.deltaOnce.Do(func() {
		b.delta, b.deltaErr = newContainerdEngine(b.c.conn)
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
// a per-spawn image (base layers + delta layer, lease-pinned), validates the manifest references
// the delta descriptor (moby#47065 guard), and removes the container. Returns the assembled image
// ref ("spawnery/delta:<spawnID>").
func (b *CRIPodBackend) CaptureDelta(ctx context.Context, h *runtime.PodHandle) (string, error) {
	eng, err := b.engine()
	if err != nil {
		return "", fmt.Errorf("cri delta engine: %w", err)
	}
	if h.AgentID == "" {
		return "", fmt.Errorf("cri capture: no agent container id")
	}

	name := runtime.DeltaTag(h.SpawnID)
	leaseID := deltaLeaseID(h.SpawnID)

	// Stop the container (not remove) so its snapshot is quiesced before diff.
	if _, err := b.c.runtime.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: h.AgentID}); err != nil {
		return "", fmt.Errorf("cri capture stop %s: %w", h.AgentID, err)
	}

	ref, deltaDigest, layers, err := eng.Capture(ctx, h.AgentID, name, h.BaseImageRef, leaseID)
	if err != nil {
		return "", fmt.Errorf("cri capture %s: %w", h.SpawnID, err)
	}

	// moby#47065-class guard: assembled manifest must reference the delta layer.
	if !slices.Contains(layers, deltaDigest) {
		_ = eng.Release(context.WithoutCancel(ctx), name, leaseID)
		return "", fmt.Errorf("cri capture %s: assembled manifest does not reference delta layer %s (moby#47065 guard)",
			h.SpawnID, deltaDigest)
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
