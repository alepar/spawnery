package fakepod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"spawnery/internal/runtime"
)

// ImageInfo is the fake's view of one image in the chain.
type ImageInfo struct {
	Ref        string
	Base       string // parent ref ("" for a base image)
	Layers     int
	Depth      int
	Launchable bool // a delta committed but never made launchable (the "never Unpacked into the
	// snapshotter" bug) must NOT be returned by EnsureImage
}

type image struct {
	ImageInfo
	content map[string][]byte // the rootfs snapshot committed into this image
}

// ensureBaseImage registers ref as a launchable 1-layer base image if the fake has not seen it.
// Caller holds b.mu.
func (b *Backend) ensureBaseImage(ref string) *image {
	if ref == "" {
		return nil
	}
	if img, ok := b.images[ref]; ok {
		return img
	}
	img := &image{
		ImageInfo: ImageInfo{Ref: ref, Layers: 1, Depth: 0, Launchable: true},
		content:   map[string][]byte{},
	}
	b.images[ref] = img
	return img
}

// deltaArchive is the wire form of ExportDelta / ImportDelta.
type deltaArchive struct {
	Base    string            `json:"base"`
	Depth   int               `json:"depth"`
	Content map[string][]byte `json:"content"`
}

// Images returns a snapshot of the image chain, keyed by ref.
func (b *Backend) Images() map[string]ImageInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]ImageInfo, len(b.images))
	for ref, img := range b.images {
		out[ref] = img.ImageInfo
	}
	return out
}

// ImageContent returns a copy of the rootfs content committed into ref.
func (b *Backend) ImageContent(ref string) (map[string][]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	img, ok := b.images[ref]
	if !ok {
		return nil, false
	}
	return copyView(img.content), true
}

// MarkUnlaunchable models a delta image that exists but cannot be launched. EnsureImage must fall
// back to the base for it.
func (b *Backend) MarkUnlaunchable(ref string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if img, ok := b.images[ref]; ok {
		img.Launchable = false
	}
}

// ResolveImageDigest returns a deterministic content-addressable digest for ref (registering ref as a
// base image if unseen).
func (b *Backend) ResolveImageDigest(_ context.Context, ref string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fault(OpResolveImageDigest, ref); err != nil {
		return "", err
	}
	b.ensureBaseImage(ref)
	b.record(OpResolveImageDigest, ref, nil)
	if b.cfg.resolveDigest != "" {
		return b.cfg.resolveDigest, nil
	}
	sum := sha256.Sum256([]byte(ref))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// EnsureImage returns deltaRef only if that image exists AND is launchable; otherwise baseRef.
func (b *Backend) EnsureImage(_ context.Context, baseRef, deltaRef string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fault(OpEnsureImage, deltaRef); err != nil {
		return "", err
	}
	b.record(OpEnsureImage, deltaRef, nil)
	if b.cfg.ensureImageRef != "" {
		return b.cfg.ensureImageRef, nil
	}
	if deltaRef != "" {
		if img, ok := b.images[deltaRef]; ok && img.Launchable {
			return deltaRef, nil
		}
	}
	return baseRef, nil
}

// CaptureDelta stops+commits the agent to the spawn's delta tag (PodBackend doc: Stop removes the
// container afterwards).
func (b *Backend) CaptureDelta(ctx context.Context, h *runtime.PodHandle) (string, error) {
	return b.capture(ctx, h, "", OpCaptureDelta)
}

// CaptureDeltaAs commits the SOURCE agent's writable layer to targetSpawnID's delta tag WITHOUT
// stopping or removing the source. A fork that destroys its own source is exactly the bug this models.
func (b *Backend) CaptureDeltaAs(ctx context.Context, h *runtime.PodHandle, targetSpawnID string) (string, error) {
	return b.capture(ctx, h, targetSpawnID, OpCaptureDeltaAs)
}

func (b *Backend) capture(_ context.Context, h *runtime.PodHandle, target string, op Op) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, err := b.lookup(h)
	if err != nil {
		return "", fmt.Errorf("fakepod: %s: %w", op, err)
	}
	if target == "" {
		target = h.SpawnID
	}
	if target == "" {
		target = p.spawnID
	}
	if ferr := b.fault(op, target); ferr != nil {
		return "", ferr
	}
	if p.agent == nil {
		return "", fmt.Errorf("fakepod: %s: pod %q has no agent", op, p.spawnID)
	}
	// Capture is legal only on a LIVE container (running or paused). A stopped/removed agent means the
	// caller destroyed its own source — that must FAIL, not silently produce an empty artifact.
	if err := p.agent.requireState(string(op), StateRunning, StatePaused); err != nil {
		return "", err
	}

	baseLayers, baseDepth := 0, 0
	if base, ok := b.images[h.BaseImageRef]; ok {
		baseLayers, baseDepth = base.Layers, base.Depth
	}
	layers := baseLayers + 1
	if b.cfg.zeroLayerCapture {
		layers = baseLayers
	}
	if layers <= baseLayers {
		return "", fmt.Errorf("fakepod: delta capture for %s produced %d layers <= base %d "+
			"(moby#47065 zero-layer guard)", target, layers, baseLayers)
	}

	ref := runtime.DeltaTag(target)
	b.images[ref] = &image{
		ImageInfo: ImageInfo{Ref: ref, Base: h.BaseImageRef, Layers: layers, Depth: baseDepth + 1, Launchable: true},
		content:   copyView(p.rootfs), // a COPY, taken at THIS instant
	}
	if op == OpCaptureDelta {
		if err := p.agent.transition(StateStopped); err != nil {
			return "", err
		}
	}
	// CaptureDeltaAs deliberately leaves the source's state and content untouched.
	b.capturedRefs = append(b.capturedRefs, ref)
	b.record(op, target, nil)
	return ref, nil
}

// ReleaseDelta removes the per-spawn delta tag.
func (b *Backend) ReleaseDelta(_ context.Context, spawnID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fault(OpReleaseDelta, spawnID); err != nil {
		return err
	}
	delete(b.images, runtime.DeltaTag(spawnID))
	b.releasedSpawns = append(b.releasedSpawns, spawnID)
	b.record(OpReleaseDelta, spawnID, nil)
	return nil
}

// ExportDelta streams the spawn's delta image (base ref, depth, content) as JSON.
func (b *Backend) ExportDelta(_ context.Context, spawnID string, w io.Writer) error {
	b.mu.Lock()
	if err := b.fault(OpExportDelta, spawnID); err != nil {
		b.mu.Unlock()
		return err
	}
	ref := runtime.DeltaTag(spawnID)
	img, ok := b.images[ref]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("fakepod: ExportDelta: delta image %s not found", ref)
	}
	arc := deltaArchive{Base: img.Base, Depth: img.Depth, Content: copyView(img.content)}
	b.record(OpExportDelta, spawnID, nil)
	b.mu.Unlock()
	return json.NewEncoder(w).Encode(arc)
}

// ImportDelta loads an exported delta onto this backend under the deterministic per-spawn tag.
func (b *Backend) ImportDelta(_ context.Context, spawnID, baseRef string, r io.Reader) (string, error) {
	var arc deltaArchive
	if err := json.NewDecoder(r).Decode(&arc); err != nil {
		return "", fmt.Errorf("fakepod: ImportDelta: decode: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fault(OpImportDelta, spawnID); err != nil {
		return "", err
	}
	baseLayers := 0
	if base := b.ensureBaseImage(baseRef); base != nil {
		baseLayers = base.Layers
	}
	if arc.Content == nil {
		arc.Content = map[string][]byte{}
	}
	ref := runtime.DeltaTag(spawnID)
	b.images[ref] = &image{
		ImageInfo: ImageInfo{Ref: ref, Base: baseRef, Layers: baseLayers + 1, Depth: arc.Depth, Launchable: true},
		content:   arc.Content,
	}
	b.importBaseRefs = append(b.importBaseRefs, baseRef)
	b.record(OpImportDelta, spawnID, nil)
	return ref, nil
}

// DeltaSize implements the Manager's optional deltaSizer: the byte size of the spawn's captured delta
// content (0 for an unknown spawn — "unknown size", which is safe to emit).
func (b *Backend) DeltaSize(_ context.Context, spawnID string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg.deltaSizeBytes > 0 {
		return b.cfg.deltaSizeBytes, nil
	}
	img, ok := b.images[runtime.DeltaTag(spawnID)]
	if !ok {
		return 0, nil
	}
	var n int64
	for _, v := range img.content {
		n += int64(len(v))
	}
	return n, nil
}
