package spawnlet

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/storage"
)

type fakeRootMaterializer struct {
	calls []runtime.RootMaterializeSpec
}

func (m *fakeRootMaterializer) MaterializeRootOwned(_ context.Context, spec runtime.RootMaterializeSpec) error {
	m.calls = append(m.calls, spec)
	if err := os.RemoveAll(spec.TargetPath); err != nil {
		return err
	}
	if err := os.MkdirAll(spec.TargetPath, spec.DirMode); err != nil {
		return err
	}
	return filepath.WalkDir(spec.SourcePath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == spec.SourcePath {
			return nil
		}
		rel, err := filepath.Rel(spec.SourcePath, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(spec.TargetPath, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, spec.DirMode)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, spec.FileMode)
	})
}

type countingPodBackend struct {
	fakePodBackend
	startPodCalls   int
	startAgentCalls int
}

func (b *countingPodBackend) StartPod(ctx context.Context, spec runtime.PodSpec) (*runtime.PodHandle, error) {
	b.startPodCalls++
	return b.fakePodBackend.StartPod(ctx, spec)
}

func (b *countingPodBackend) StartAgent(ctx context.Context, h *runtime.PodHandle, spec runtime.AgentSpec) error {
	b.startAgentCalls++
	return b.fakePodBackend.StartAgent(ctx, h, spec)
}

type recordingBackend struct {
	root      string
	prepared  []string
	finalized []string
}

func (b *recordingBackend) Prepare(_ context.Context, spawnID, mountName, seedDir string, agentUID int) (string, error) {
	hostDir := filepath.Join(b.root, spawnID, mountName)
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return "", err
	}
	b.prepared = append(b.prepared, hostDir)
	return hostDir, nil
}

func (b *recordingBackend) Finalize(_ context.Context, hostDir string) error {
	b.finalized = append(b.finalized, hostDir)
	return nil
}

type recordingResolver struct {
	backends map[string]storage.Backend
}

func (r recordingResolver) Resolve(backendURI string) (storage.Backend, error) {
	backend, ok := r.backends[backendURI]
	if !ok {
		return nil, fmt.Errorf("unexpected backend URI %q", backendURI)
	}
	return backend, nil
}

func writeMountBindingApp(t *testing.T, mounts ...string) string {
	t.Helper()

	app := t.TempDir()
	var manifest strings.Builder
	manifest.WriteString("id: spawnery/mount-bindings\nstorage:\n  mounts:\n")
	for _, mountName := range mounts {
		manifest.WriteString(fmt.Sprintf("    - name: %s\n      path: %s\n      seed: %s\n", mountName, mountName, mountName))
		if err := os.MkdirAll(filepath.Join(app, mountName), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(app, "spawneryapp.yml"), []byte(manifest.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestCreateWithSelectionRejectsUnsupportedMountBackendBeforePodStart(t *testing.T) {
	fb := &countingPodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	_, err := m.CreateWithSelection(context.Background(), "sp-github", writeMountBindingApp(t, "main"), "model", "", "", 0, AgentSelection{
		Mounts: []MountBinding{{Name: "main", BackendURI: "github:owner/repo"}},
	})
	if !errors.Is(err, storage.ErrUnsupportedBackend) {
		t.Fatalf("CreateWithSelection error = %v, want ErrUnsupportedBackend", err)
	}
	if fb.startPodCalls != 0 || fb.startAgentCalls != 0 {
		t.Fatalf("pod should not start on unsupported backend, got StartPod=%d StartAgent=%d", fb.startPodCalls, fb.startAgentCalls)
	}
}

func TestCreateWithSelectionRejectsBindingForUnknownManifestMount(t *testing.T) {
	fb := &countingPodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	_, err := m.CreateWithSelection(context.Background(), "sp-unknown-binding", writeMountBindingApp(t, "main"), "model", "", "", 0, AgentSelection{
		Mounts: []MountBinding{{Name: "cache", BackendURI: "scratch:"}},
	})
	if err == nil {
		t.Fatal("CreateWithSelection should reject mount bindings for undeclared manifest mounts")
	}
	if !strings.Contains(err.Error(), "cache") || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("CreateWithSelection error = %q, want undeclared mount detail", err)
	}
	if fb.startPodCalls != 0 || fb.startAgentCalls != 0 {
		t.Fatalf("pod should not start on invalid binding, got StartPod=%d StartAgent=%d", fb.startPodCalls, fb.startAgentCalls)
	}
}

func TestStopFinalizesEachMountThroughPreparingBackend(t *testing.T) {
	fb := &countingPodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	scratchBackend := &recordingBackend{root: filepath.Join(t.TempDir(), "scratch")}
	githubBackend := &recordingBackend{root: filepath.Join(t.TempDir(), "github")}
	m.backendResolver = recordingResolver{
		backends: map[string]storage.Backend{
			"":                  scratchBackend,
			"github:owner/repo": githubBackend,
		},
	}

	sp, err := m.CreateWithSelection(context.Background(), "sp-finalizers", writeMountBindingApp(t, "main", "cache"), "model", "", "", 0, AgentSelection{
		Mounts: []MountBinding{{Name: "main", BackendURI: "github:owner/repo"}},
	})
	if err != nil {
		t.Fatalf("CreateWithSelection: %v", err)
	}

	if err := m.Stop(context.Background(), sp.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(githubBackend.prepared) != 1 || len(githubBackend.finalized) != 1 || githubBackend.finalized[0] != githubBackend.prepared[0] {
		t.Fatalf("github backend prepare/finalize = %v/%v", githubBackend.prepared, githubBackend.finalized)
	}
	if len(scratchBackend.prepared) == 0 || len(scratchBackend.finalized) == 0 {
		t.Fatalf("scratch backend prepare/finalize = %v/%v", scratchBackend.prepared, scratchBackend.finalized)
	}
}

func TestRootMaterializeUsesResolvedBackendForMountPrepareAndFinalize(t *testing.T) {
	fb := &countingPodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), UsernsMode: "remap",
	})
	m.rootMaterializer = &fakeRootMaterializer{}

	githubBackend := &recordingBackend{root: filepath.Join(t.TempDir(), "github")}
	m.backendResolver = recordingResolver{
		backends: map[string]storage.Backend{
			"":                  &recordingBackend{root: filepath.Join(t.TempDir(), "scratch")},
			"github:owner/repo": githubBackend,
		},
	}

	sp, err := m.CreateWithSelection(context.Background(), "sp-root-remap", writeMountBindingApp(t, "main"), "model", "", "", 0, AgentSelection{
		Mounts: []MountBinding{{Name: "main", BackendURI: "github:owner/repo"}},
	})
	if err != nil {
		t.Fatalf("CreateWithSelection: %v", err)
	}

	if err := m.Stop(context.Background(), sp.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(githubBackend.prepared) != 1 {
		t.Fatalf("github backend prepared %v, want exactly one mount prepare", githubBackend.prepared)
	}
	if len(githubBackend.finalized) != 1 || githubBackend.finalized[0] != githubBackend.prepared[0] {
		t.Fatalf("github backend finalize = %v, want finalize of prepared dir %v", githubBackend.finalized, githubBackend.prepared)
	}
}
