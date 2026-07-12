//go:build cri_delta_e2e

// The CRI arm of the PodBackend contract (sp-2tx8.1.3). Runs the same RunContract table the fake and the
// Docker lane run, against the real CRIPodBackend on a real containerd (CRI plugin + CNI + runc/runsc).
//
// Needs: root, a CRI-capable containerd, CNI, and the fixture images in the k8s.io namespace. The
// `just test-cri-contract` recipe provides all of it. Per project convention the build tag IS the opt-in,
// so a missing dep is a FAILURE, never a skip.
//
// Env:
//
//	CONTAINERD_ADDRESS  CRI socket           (default /run/containerd/containerd.sock)
//	RUNTIME_HANDLER     CRI runtime handler  (default "runc"; "runsc" for the gVisor lane)
//	AGENT_IMAGE         agent/base image     (default spawnery/stubagent:dev)
//	SIDECAR_IMAGE       sidecar image        (default spawnery/sidecar:dev)
//
// LANE DIVERGENCES recorded here (see the SE1 spec §4.3):
//   - AgentSpec.Cmd maps to CRI `Command`, which overrides the image ENTRYPOINT (the Docker lane's maps
//     to Config.Cmd, which overrides CMD and keeps ENTRYPOINT). This arm drives Cmd=nil, so both lanes
//     fall through to the image's own entrypoint and the divergence is not exercised.
//   - ListManaged reports SidecarID+AgentID+PodIP on both lanes (sp-2tx8.3.1); CRI additionally reports
//     SandboxID, which Docker has no analogue for and leaves empty. The contract asserts the ids and the
//     IP round-trip; SandboxID is not asserted.
//   - The zero-layer guard is NOT a layer count here: CaptureDeltaAs rejects a capture whose delta layer
//     is empty (deltaSize <= 0) and releases the half-made image. ArmZeroLayerCapture below forces that
//     condition through the deltaEngine seam.
package cri

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/podbackendtest"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const criRootfsFile = "/contract-marker"

func criEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// armableEngine delegates to the real containerdEngine, but while armed reports the captured delta as
// EMPTY (size 0) — the observable symptom of a corrupt/empty CreateDiff. CaptureDeltaAs must then reject
// the capture and release the half-made image rather than hand resume a delta with none of the agent's
// writes. (There is no way to make containerd emit an empty diff on cue; this seam is the Option the
// backend already exposes for tests.)
type armableEngine struct {
	deltaEngine
	mu    sync.Mutex
	armed bool
}

func (a *armableEngine) arm(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.armed = on
}

func (a *armableEngine) Capture(ctx context.Context, snapshotKey, name, baseRef, leaseID string) (string, int64, error) {
	ref, size, err := a.deltaEngine.Capture(ctx, snapshotKey, name, baseRef, leaseID)
	a.mu.Lock()
	armed := a.armed
	a.mu.Unlock()
	if armed && err == nil {
		return ref, 0, nil
	}
	return ref, size, err
}

// criExec runs argv in a container and fails on a non-zero exit. ExecSync carries its own timeout, so a
// PAUSED container (whose exec can never run) produces an error rather than a hang — which is what the
// contract's Exec/Write hooks require.
func criExec(ctx context.Context, c *Client, containerID string, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.runtime.ExecSync(ctx, &runtimeapi.ExecSyncRequest{
		ContainerId: containerID,
		Cmd:         argv,
		Timeout:     15,
	})
	if err != nil {
		return nil, fmt.Errorf("exec %v in %s: %w", argv, containerID, err)
	}
	if resp.GetExitCode() != 0 {
		return nil, fmt.Errorf("exec %v exited %d: %s", argv, resp.GetExitCode(),
			strings.TrimSpace(string(resp.GetStderr())))
	}
	return resp.GetStdout(), nil
}

func criShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func criUniqueID(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// criFactory builds a fresh Env per contract case.
func criFactory(t *testing.T) *podbackendtest.Env {
	t.Helper()
	addr := criEnvOr("CONTAINERD_ADDRESS", "/run/containerd/containerd.sock")
	handler := criEnvOr("RUNTIME_HANDLER", "runc")
	agentImage := criEnvOr("AGENT_IMAGE", "spawnery/stubagent:dev")
	sidecarImage := criEnvOr("SIDECAR_IMAGE", "spawnery/sidecar:dev")

	c, err := Dial("unix://" + addr)
	if err != nil {
		t.Fatalf("CRI not reachable at %s: %v (run `just test-cri-contract`, which stands up a "+
			"CRI-capable containerd with CNI)", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })

	eng, err := newContainerdEngine(c.conn)
	if err != nil {
		t.Fatalf("containerd delta engine over %s: %v", addr, err)
	}
	armable := &armableEngine{deltaEngine: eng}

	backend := NewCRIPodBackend(c, handler, WithDeltaEngine(armable))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := backend.Ping(ctx); err != nil {
		t.Fatalf("CRI status at %s: %v (containerd is up but the CRI plugin is not serving)", addr, err)
	}
	if err := backend.Preflight(ctx); err != nil {
		t.Fatalf("CRI preflight at %s: %v (RuntimeReady/NetworkReady false — is the CNI conflist in "+
			"the configured conf_dir?)", addr, err)
	}
	if _, err := backend.ResolveImageDigest(ctx, agentImage); err != nil {
		t.Fatalf("agent image %s is not in the CRI image store: %v (import it: "+
			"`ctr -n k8s.io images import`, see `just test-cri-contract`)", agentImage, err)
	}
	if _, err := backend.ResolveImageDigest(ctx, sidecarImage); err != nil {
		t.Fatalf("sidecar image %s is not in the CRI image store: %v", sidecarImage, err)
	}

	podSpec := func(spawnID string, labels map[string]string) runtime.PodSpec {
		return runtime.PodSpec{
			ID:           spawnID,
			SidecarImage: sidecarImage,
			SidecarEnv:   []string{"OPENROUTER_API_KEY=unused-by-the-contract"},
			Runtime:      handler,
			Labels:       labels,
		}
	}
	// Cmd nil => the stubagent image's own entrypoint (launcher → acpmux on tcp/7000).
	agentSpec := func(_, imageRef string, labels map[string]string) runtime.AgentSpec {
		return runtime.AgentSpec{Image: imageRef, Runtime: handler, Labels: labels}
	}

	return &podbackendtest.Env{
		Backend:    backend,
		NodeID:     "node-ct-" + criUniqueID(t),
		BaseImage:  agentImage,
		RootfsFile: criRootfsFile,
		PodSpec:    podSpec,
		AgentSpec:  agentSpec,
		Write: func(ctx context.Context, h *runtime.PodHandle, file string, data []byte) error {
			_, err := criExec(ctx, c, h.AgentID, []string{
				"/bin/sh", "-c", "printf %s " + criShellQuote(string(data)) + " > " + criShellQuote(file),
			})
			return err
		},
		ReadArtifact: func(ctx context.Context, ref, file string) ([]byte, error) {
			return criReadArtifact(ctx, t, backend, podSpec, agentSpec, c, ref, file)
		},
		Exec: func(ctx context.Context, h *runtime.PodHandle, argv []string) error {
			_, err := criExec(ctx, c, h.AgentID, argv)
			return err
		},
		ArmZeroLayerCapture: func() func() {
			armable.arm(true)
			return func() { armable.arm(false) }
		},
		// No exceptions: the CRI lane is expected to support the whole contract. If one turns out to be
		// unsupportable, register it HERE with a reason — never weaken the contract case.
		Exceptions: nil,
	}
}

// criReadArtifact launches a throwaway pod from the captured delta image and cats the file out of it.
// Reading the image content directly would mean re-implementing the snapshotter mount; launching it is
// both simpler and sharper — an artifact that cannot be launched is a failure we WANT to see here.
func criReadArtifact(
	ctx context.Context,
	t *testing.T,
	b *CRIPodBackend,
	podSpec func(string, map[string]string) runtime.PodSpec,
	agentSpec func(string, string, map[string]string) runtime.AgentSpec,
	c *Client,
	ref, file string,
) ([]byte, error) {
	t.Helper()
	id := "rd" + criUniqueID(t)
	labels := podbackendtest.Labels(id, "node-readartifact", 1)
	h, err := b.StartPod(ctx, podSpec(id, labels))
	if err != nil {
		return nil, fmt.Errorf("read-artifact pod for %s: %w", ref, err)
	}
	defer func() { _ = b.Stop(context.WithoutCancel(ctx), h) }()
	h.SpawnID = id
	if err := b.StartAgent(ctx, h, agentSpec(id, ref, labels)); err != nil {
		return nil, fmt.Errorf("launch the captured delta %s: %w", ref, err)
	}
	out, err := criExec(ctx, c, h.AgentID, []string{"/bin/cat", file})
	if err != nil {
		return nil, fmt.Errorf("cat %s inside %s: %w", file, ref, err)
	}
	return bytes.TrimSpace(out), nil
}

func TestPodBackendContract_CRI(t *testing.T) {
	podbackendtest.RunContract(t, criFactory)
}
