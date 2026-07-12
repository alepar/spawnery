package podbackendtest_test

import (
	"context"
	"fmt"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/fakepod"
	"spawnery/internal/runtime/podbackendtest"
)

const (
	fakeNodeID       = "node-contract"
	fakeSidecarImage = "spawnery/sidecar:test"
	fakeAgentImage   = "spawnery/agent:test"
	// fakeRootfsFile is in the writable layer and NOT under fakeMountPath — so it is what a delta
	// capture commits.
	fakeRootfsFile = "/work/contract-marker"
	fakeMountPath  = "/mnt/scratch"
)

// fakeFactory builds a fresh fakepod-backed Env. RunContract calls it once per case.
func fakeFactory(t *testing.T) *podbackendtest.Env {
	t.Helper()
	b := fakepod.New(
		fakepod.WithBaseImage(fakeAgentImage, 3),
		fakepod.WithBaseImage(fakeSidecarImage, 2),
	)
	t.Cleanup(b.Close)

	mounts := []runtime.Mount{{HostPath: t.TempDir(), ContainerPath: fakeMountPath}}

	return &podbackendtest.Env{
		Backend:    b,
		NodeID:     fakeNodeID,
		BaseImage:  fakeAgentImage,
		RootfsFile: fakeRootfsFile,
		PodSpec: func(spawnID string, labels map[string]string) runtime.PodSpec {
			return runtime.PodSpec{
				ID:           spawnID,
				SidecarImage: fakeSidecarImage,
				Labels:       labels,
			}
		},
		AgentSpec: func(_, imageRef string, labels map[string]string) runtime.AgentSpec {
			return runtime.AgentSpec{
				Image:  imageRef,
				Mounts: mounts,
				Labels: labels,
			}
		},
		Write: func(_ context.Context, h *runtime.PodHandle, file string, data []byte) error {
			return b.AgentWrite(h.SpawnID, file, data)
		},
		ReadArtifact: func(_ context.Context, ref, file string) ([]byte, error) {
			content, ok := b.ImageContent(ref)
			if !ok {
				return nil, fmt.Errorf("no such image %q", ref)
			}
			data, ok := content[file]
			if !ok {
				return nil, fmt.Errorf("image %q has no %q", ref, file)
			}
			return data, nil
		},
		Exec: func(ctx context.Context, h *runtime.PodHandle, argv []string) error {
			return b.Exec(ctx, h.AgentID, argv)
		},
		ArmZeroLayerCapture: func() func() {
			b.SetZeroLayerCapture(true)
			return func() { b.SetZeroLayerCapture(false) }
		},
		// The fake supports the whole contract. Any exception here is a fidelity gap — see
		// TestFakepodRegistersNoExceptions.
		Exceptions: nil,
	}
}

// TestPodBackendContract_Fakepod is the hermetic arm: the contract runs against the in-memory backend
// on every `go test ./...`, with no Docker, no containerd and no images.
func TestPodBackendContract_Fakepod(t *testing.T) {
	podbackendtest.RunContract(t, fakeFactory)
}

// TestFakepodRegistersNoExceptions: the fake is the reference implementation of the contract. An
// exception in the fake's Env means the fake models something the contract says it must not — that is
// a bug in the fake, not a lane quirk, and it must be argued for explicitly.
func TestFakepodRegistersNoExceptions(t *testing.T) {
	if ex := fakeFactory(t).Exceptions; len(ex) != 0 {
		t.Fatalf("the fakepod arm registered contract exceptions %v — the fake is the reference "+
			"implementation and must satisfy the whole contract", ex)
	}
}
