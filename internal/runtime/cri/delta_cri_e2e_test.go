//go:build cri_delta_e2e

// Live containerd round-trip for the CRI lane's delta-only export/import (sp-ei4.1.16 / .14).
//
// Verifies the REAL containerdEngine end to end on a running containerd with the overlayfs
// snapshotter and the runc runtime: derive a writable snapshot from a base image, make a
// deletion (overlay whiteout) + an added file, Capture it into a per-spawn delta image, export
// ONLY the top layer (assert uncompressed for Kopia dedup), drop the delta image, AssembleOnBase
// the shipped layer back onto the base, then UNPACK + RUN the reassembled image and assert the
// whiteout took effect and the added file survived. The unpack is the load-bearing check: it
// proves containerd's snapshotter materializes our uncompressed-tar delta layer correctly (the
// same snapshotter prepares the rootfs runsc boots on, so this covers the runsc lane's risk).
//
// Run via `just test-cri-delta` (stands up a dedicated containerd as root). Env:
//
//	CONTAINERD_ADDRESS  containerd gRPC socket (default /run/containerd/containerd.sock)
//	BASE_IMAGE          base image present+unpackable in ns k8s.io (default docker.io/library/debian:stable)
package cri

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	ctrclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/image-spec/identity"
)

const e2eSnapshotter = "overlayfs"

func TestCRIDeltaOnlyRoundTrip(t *testing.T) {
	addr := os.Getenv("CONTAINERD_ADDRESS")
	if addr == "" {
		addr = "/run/containerd/containerd.sock"
	}
	baseRef := os.Getenv("BASE_IMAGE")
	if baseRef == "" {
		baseRef = "docker.io/library/debian:stable"
	}

	// runsc/CRI lane test: skip (not fail) when the lane's deps (containerd/runsc, base image in
	// the k8s.io namespace) are absent — this lane is not present in every environment by design
	// (CLAUDE.md lane-specific exception). Non-lane shared deps (Docker, Garage) still fail.
	cl, err := ctrclient.New(addr, ctrclient.WithDefaultNamespace("k8s.io"))
	if err != nil {
		t.Skipf("containerd not reachable at %s: %v", addr, err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := cl.Version(ctx); err != nil {
		t.Skipf("containerd ping failed: %v", err)
	}

	base, err := cl.GetImage(ctx, baseRef)
	if err != nil {
		t.Skipf("base image %s not present (pull it into ns k8s.io first): %v", baseRef, err)
	}
	if err := base.Unpack(ctx, e2eSnapshotter); err != nil {
		t.Fatalf("unpack base: %v", err)
	}

	eng := &containerdEngine{client: cl}
	const (
		snapKey  = "spawnery-cde2e-rw"
		deltaTag = "spawnery/delta:cde2e"
		outTag   = "spawnery/delta-reassembled:cde2e"
		leaseA   = "spawnery-cde2e-a"
		leaseB   = "spawnery-cde2e-b"
	)

	// 1. Writable snapshot from the base + a runc task that deletes a base file (→ overlay
	//    whiteout) and adds a new one. Keep the snapshot after the task exits so Capture can diff it.
	c, err := cl.NewContainer(ctx, "spawnery-cde2e",
		ctrclient.WithSnapshotter(e2eSnapshotter),
		ctrclient.WithNewSnapshot(snapKey, base),
		ctrclient.WithNewSpec(oci.WithImageConfig(base),
			oci.WithProcessArgs("sh", "-c", "rm -f /etc/os-release && echo from-delta > /delta-added.txt && sync")),
	)
	if err != nil {
		t.Fatalf("new container: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), ctrclient.WithSnapshotCleanup) })
	task, err := c.NewTask(ctx, cio.NullIO)
	if err != nil {
		t.Fatalf("new task: %v", err)
	}
	statusC, err := task.Wait(ctx)
	if err != nil {
		t.Fatalf("task wait: %v", err)
	}
	if err := task.Start(ctx); err != nil {
		t.Fatalf("task start: %v", err)
	}
	st := <-statusC
	if code := st.ExitCode(); code != 0 {
		t.Fatalf("delta-producing task exited %d", code)
	}
	if _, err := task.Delete(ctx); err != nil { // keeps the snapshot
		t.Fatalf("task delete: %v", err)
	}

	// 2. Capture → per-spawn delta image (base layers + the diffed delta layer).
	if _, _, err := eng.Capture(ctx, snapKey, deltaTag, baseRef, leaseA); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// 3. Export ONLY the top layer; assert it is an uncompressed tar (not gzip → Kopia CDC dedup).
	var layer bytes.Buffer
	if err := eng.ExportTopLayer(ctx, deltaTag, &layer); err != nil {
		t.Fatalf("ExportTopLayer: %v", err)
	}
	if layer.Len() == 0 {
		t.Fatal("exported delta layer is empty")
	}
	if bytes.HasPrefix(layer.Bytes(), []byte{0x1f, 0x8b}) {
		t.Fatal("exported delta layer is gzip; must be uncompressed for Kopia CDC dedup")
	}
	t.Logf("exported delta top layer: %d bytes (uncompressed tar)", layer.Len())

	// 4. Drop the delta image record (simulate: target node has only the pinned base) and
	//    reassemble base + the shipped layer into a fresh image.
	_ = eng.Release(ctx, deltaTag, leaseA)
	if err := eng.AssembleOnBase(ctx, baseRef, outTag, leaseB, bytes.NewReader(layer.Bytes())); err != nil {
		t.Fatalf("AssembleOnBase: %v", err)
	}
	t.Cleanup(func() { _ = eng.Release(context.Background(), outTag, leaseB) })

	// 5. THE LOAD-BEARING CHECK: unpack the reassembled image (snapshotter must materialize the
	//    uncompressed-tar delta layer) and run it — whiteout applied, added file present.
	out, err := cl.GetImage(ctx, outTag)
	if err != nil {
		t.Fatalf("get reassembled image: %v", err)
	}
	if err := out.Unpack(ctx, e2eSnapshotter); err != nil {
		t.Fatalf("unpack reassembled image (uncompressed delta layer): %v", err)
	}
	// Sanity: the reassembled chain = base chain + 1.
	baseDiffs, _ := base.RootFS(ctx)
	outDiffs, _ := out.RootFS(ctx)
	if len(outDiffs) != len(baseDiffs)+1 {
		t.Fatalf("reassembled diffIDs = %d, want base+1 = %d", len(outDiffs), len(baseDiffs)+1)
	}
	_ = identity.ChainID(outDiffs) // chainID computes → layers form a valid chain

	vc, err := cl.NewContainer(ctx, "spawnery-cde2e-verify",
		ctrclient.WithSnapshotter(e2eSnapshotter),
		ctrclient.WithNewSnapshot("spawnery-cde2e-verify-rw", out),
		ctrclient.WithNewSpec(oci.WithImageConfig(out),
			oci.WithProcessArgs("sh", "-c", "test ! -e /etc/os-release && test \"$(cat /delta-added.txt)\" = from-delta")),
	)
	if err != nil {
		t.Fatalf("new verify container: %v", err)
	}
	t.Cleanup(func() { _ = vc.Delete(context.Background(), ctrclient.WithSnapshotCleanup) })
	vt, err := vc.NewTask(ctx, cio.NullIO)
	if err != nil {
		t.Fatalf("new verify task: %v", err)
	}
	vstatusC, _ := vt.Wait(ctx)
	if err := vt.Start(ctx); err != nil {
		t.Fatalf("verify task start: %v", err)
	}
	vst := <-vstatusC
	if _, err := vt.Delete(ctx); err != nil {
		t.Fatalf("verify task delete: %v", err)
	}
	if code := vst.ExitCode(); code != 0 {
		t.Fatalf("reassembled rootfs failed fidelity check (exit %d): whiteout not applied or added file missing", code)
	}
	t.Logf("reassembled image OK: whiteout applied + added file present (%s = base+delta)", outTag)
}
