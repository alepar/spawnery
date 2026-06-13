package cri

// containerd.go — thin raw containerd engine. Untested by unit tests; e2e covers this path.
// All orchestration logic (stop order, moby guard, lease naming) lives in delta.go and is
// unit-tested there via a fakeDeltaEngine.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ctrclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	pkgrootfs "github.com/containerd/containerd/v2/pkg/rootfs"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
)

// defaultSnapshotter is the snapshotter name used for CRI containers under containerd.
// runsc with overlay2=none uses the standard overlayfs snapshotter on the host side.
// NOTE: runsc handler snapshotter must be confirmed in e2e (task sp-ei4.1 follow-up).
const defaultSnapshotter = "overlayfs"

// containerdEngine builds OCI delta images using the native containerd APIs over the shared
// CRI gRPC connection. It implements deltaEngine. Raw containerd calls are kept thin here;
// all orchestration (ordering, guard, lease naming) is in delta.go and unit-tested there.
type containerdEngine struct {
	client *ctrclient.Client
}

// newContainerdEngine creates a containerdEngine from the existing CRI gRPC connection.
// No new dial is performed; the containerd client is multiplexed on top of the same socket.
// NewWithConn does no I/O, so this constructor is I/O-free (safe for lazy-init).
func newContainerdEngine(conn *grpc.ClientConn) (deltaEngine, error) {
	c, err := ctrclient.NewWithConn(conn, ctrclient.WithDefaultNamespace("k8s.io"))
	if err != nil {
		return nil, fmt.Errorf("containerd client from conn: %w", err)
	}
	return &containerdEngine{client: c}, nil
}

// Close is intentionally a no-op. The containerd Client was constructed from the shared CRI
// gRPC connection which is owned and closed by cri.Client. Calling c.client.Close() would
// close the shared conn and break all subsequent CRI calls on the backend.
func (e *containerdEngine) Close() error { return nil }

// Capture diffs the rw snapshot for snapshotKey (== the CRI container id, k8s.io ns) against
// its parent chain, assembles a new image `name` (base layers + delta layer) pinned by lease
// leaseID, and returns (name, deltaLayerSize, err). deltaLayerSize is the compressed byte size
// of the produced delta blob; a zero value means the diff was empty.
// On any error a best-effort lease deletion is attempted to prevent orphaned blobs.
func (e *containerdEngine) Capture(ctx context.Context, snapshotKey, name, baseRef, leaseID string) (string, int64, error) {
	leaseMgr := e.client.LeasesService()

	// Create the lease (idempotent: ignore AlreadyExists).
	if _, err := leaseMgr.Create(ctx, leases.WithID(leaseID)); err != nil && !errdefs.IsAlreadyExists(err) {
		return "", 0, fmt.Errorf("create lease %s: %w", leaseID, err)
	}

	// All subsequent writes to the content store are automatically attached to this lease.
	ctx = leases.WithLease(ctx, leaseID)

	// Diff the active snapshot against its parent chain; result is an OCI layer tar in the
	// content store (gzip-compressed, labeled with containerd.io/uncompressed for GetDiffID).
	sn := e.client.SnapshotService(defaultSnapshotter)
	diffSvc := e.client.DiffService()
	deltaDesc, err := pkgrootfs.CreateDiff(ctx, snapshotKey, sn, diffSvc)
	if err != nil {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("create diff for snapshot %s: %w", snapshotKey, err)
	}

	// Derive the uncompressed diffID (OCI image config rootfs.diff_ids uses uncompressed digests).
	cs := e.client.ContentStore()
	diffID, err := images.GetDiffID(ctx, cs, deltaDesc)
	if err != nil {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("get diffid for delta layer: %w", err)
	}

	// Read the base image manifest and config.
	baseImg, err := e.client.ImageService().Get(ctx, baseRef)
	if err != nil {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("get base image %s: %w", baseRef, err)
	}
	baseMfst, err := images.Manifest(ctx, cs, baseImg.Target, platforms.Default())
	if err != nil {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("read base manifest: %w", err)
	}
	cfgBlob, err := content.ReadBlob(ctx, cs, baseMfst.Config)
	if err != nil {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("read base config: %w", err)
	}
	var cfg ocispec.Image
	if err := json.Unmarshal(cfgBlob, &cfg); err != nil {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("parse base config: %w", err)
	}

	// Build the new image config: append the delta diffID to RootFS.DiffIDs.
	cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, diffID)
	newCfgJSON, err := json.Marshal(cfg)
	if err != nil {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("marshal new config: %w", err)
	}
	newCfgDigest := digest.FromBytes(newCfgJSON)
	newCfgDesc := ocispec.Descriptor{
		MediaType: baseMfst.Config.MediaType,
		Digest:    newCfgDigest,
		Size:      int64(len(newCfgJSON)),
	}
	// WriteBlob is idempotent: if the blob already exists (same digest), it returns nil.
	if err := content.WriteBlob(ctx, cs, "spawnery-config-"+name, bytes.NewReader(newCfgJSON), newCfgDesc); err != nil {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("write new config: %w", err)
	}

	// Build the new manifest: base layers + the delta descriptor.
	newLayers := make([]ocispec.Descriptor, len(baseMfst.Layers)+1)
	copy(newLayers, baseMfst.Layers)
	newLayers[len(baseMfst.Layers)] = deltaDesc
	// moby#47065-class guard: assert the assembled manifest actually references the delta
	// descriptor as its top layer before we persist it. The manifest is hand-built here, so a
	// regression in the copy/append above (not a daemon bug) is the realistic failure mode; an
	// image whose layers silently drop the delta would launch the bare base and lose the spawn's
	// rootfs. This is the real reference check; delta.go's size>0 check is a separate diff-sanity
	// guard against an empty/corrupt CreateDiff.
	if last := newLayers[len(newLayers)-1]; last.Digest == "" || last.Digest != deltaDesc.Digest {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("assembled manifest does not reference delta descriptor %s (moby#47065 guard)", deltaDesc.Digest)
	}
	newMfst := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    newCfgDesc,
		Layers:    newLayers,
	}
	newMfstJSON, err := json.Marshal(newMfst)
	if err != nil {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("marshal new manifest: %w", err)
	}
	newMfstDigest := digest.FromBytes(newMfstJSON)
	newMfstDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    newMfstDigest,
		Size:      int64(len(newMfstJSON)),
	}
	if err := content.WriteBlob(ctx, cs, "spawnery-manifest-"+name, bytes.NewReader(newMfstJSON), newMfstDesc); err != nil {
		_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		return "", 0, fmt.Errorf("write new manifest: %w", err)
	}

	// Create or update the image record so the CRI image service can resolve it.
	// The io.cri-containerd.image=managed label marks it as CRI-resolvable.
	imgRecord := images.Image{
		Name:   name,
		Target: newMfstDesc,
		Labels: map[string]string{"io.cri-containerd.image": "managed"},
	}
	imgSvc := e.client.ImageService()
	if _, err := imgSvc.Create(ctx, imgRecord); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
			return "", 0, fmt.Errorf("create image record %s: %w", name, err)
		}
		if _, err := imgSvc.Update(ctx, imgRecord); err != nil {
			_ = leaseMgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
			return "", 0, fmt.Errorf("update image record %s: %w", name, err)
		}
	}

	// Return the compressed byte size of the delta blob so the caller can guard against
	// an empty/corrupt diff (moby#47065-class check in delta.go).
	return name, deltaDesc.Size, nil
}

// Release deletes the per-spawn image record and its pinning lease so the GC can reclaim the
// blobs. Ignores NotFound so a double-Release is safe.
func (e *containerdEngine) Release(ctx context.Context, name, leaseID string) error {
	var errParts []string

	imgSvc := e.client.ImageService()
	if err := imgSvc.Delete(ctx, name); err != nil && !errdefs.IsNotFound(err) {
		errParts = append(errParts, fmt.Sprintf("delete image %s: %v", name, err))
	}

	leaseMgr := e.client.LeasesService()
	if err := leaseMgr.Delete(ctx, leases.Lease{ID: leaseID}); err != nil && !errdefs.IsNotFound(err) {
		errParts = append(errParts, fmt.Sprintf("delete lease %s: %v", leaseID, err))
	}

	if len(errParts) > 0 {
		return fmt.Errorf("release delta: %s", strings.Join(errParts, "; "))
	}
	return nil
}
