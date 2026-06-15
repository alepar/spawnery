package spawnlet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet/firewall"
)

type preAgentBackend struct {
	fakePodBackend
	startAgentCalls int
	secretPath      string
	secretPlaintext []byte
	startAgentErr   error
}

func (b *preAgentBackend) StartAgent(ctx context.Context, h *runtime.PodHandle, spec runtime.AgentSpec) error {
	b.startAgentCalls++
	if b.secretPath == "" {
		return errors.New("secretPath not configured")
	}
	plaintext, err := os.ReadFile(b.secretPath)
	if err != nil {
		return err
	}
	b.secretPlaintext = plaintext
	if b.startAgentErr != nil {
		return b.startAgentErr
	}
	return b.fakePodBackend.StartAgent(ctx, h, spec)
}

func TestCreateWithSelectionRunsBeforeStartAgentAfterStartPod(t *testing.T) {
	fb := &preAgentBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	var hookCalled bool
	sp, err := m.CreateWithSelection(context.Background(), "sp-preagent", writeApp(t), "model", "", "", 3, AgentSelection{
		BeforeStartAgent: func(_ context.Context, pc PreAgentContext) error {
			hookCalled = true
			if pc.SpawnID != "sp-preagent" {
				t.Fatalf("SpawnID = %q, want sp-preagent", pc.SpawnID)
			}
			if pc.Generation != 3 {
				t.Fatalf("Generation = %d, want 3", pc.Generation)
			}
			if pc.ControlToken == "" {
				t.Fatal("ControlToken is empty")
			}
			if pc.ControlURL != "http://10.0.0.5:8081/control/model" {
				t.Fatalf("ControlURL = %q, want http://10.0.0.5:8081/control/model", pc.ControlURL)
			}
			secretPath, err := pc.InjectSecret("github/token", []byte("ghp_preagent"))
			if err != nil {
				t.Fatalf("InjectSecret: %v", err)
			}
			fb.secretPath = secretPath
			return nil
		},
	})
	if err != nil {
		t.Fatalf("CreateWithSelection: %v", err)
	}
	if !hookCalled {
		t.Fatal("BeforeStartAgent was not called")
	}
	if fb.startAgentCalls != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", fb.startAgentCalls)
	}
	if !bytes.Equal(fb.secretPlaintext, []byte("ghp_preagent")) {
		t.Fatalf("StartAgent saw secret %q, want ghp_preagent", fb.secretPlaintext)
	}
	if sp.ControlURL != "http://10.0.0.5:8081/control/model" {
		t.Fatalf("Spawn.ControlURL = %q, want http://10.0.0.5:8081/control/model", sp.ControlURL)
	}
	if _, err := os.Stat(filepath.Join(m.secrets.DirFor("sp-preagent"), "github", "token")); err != nil {
		t.Fatalf("secret file after create: %v", err)
	}
}

func TestCreateWithSelectionBeforeStartAgentFailureStopsPodAndSkipsAgent(t *testing.T) {
	hookErr := errors.New("pre-agent failed")
	fb := &preAgentBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	_, err := m.CreateWithSelection(context.Background(), "sp-preagent-fail", writeApp(t), "model", "", "", 0, AgentSelection{
		BeforeStartAgent: func(context.Context, PreAgentContext) error {
			return hookErr
		},
	})
	if !errors.Is(err, hookErr) {
		t.Fatalf("CreateWithSelection error = %v, want %v", err, hookErr)
	}
	if fb.startAgentCalls != 0 {
		t.Fatalf("StartAgent calls = %d, want 0", fb.startAgentCalls)
	}
	if fb.stopped == nil {
		t.Fatal("Stop was not called")
	}
	if fb.stopped.SidecarID != "sc" {
		t.Fatalf("stopped SidecarID = %q, want sc", fb.stopped.SidecarID)
	}
	if _, ok := m.store.Get("sp-preagent-fail"); ok {
		t.Fatal("failed pre-agent spawn was stored")
	}
}

func TestCreateWithSelectionBeforeStartAgentFailureCleanupRemovesSecretAndArtifactDirs(t *testing.T) {
	hookErr := errors.New("pre-agent failed")
	fb := &preAgentBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	_, err := m.CreateWithSelection(context.Background(), "sp-preagent-cleanup", writeApp(t), "model", "", "", 0, AgentSelection{
		BeforeStartAgent: func(_ context.Context, pc PreAgentContext) error {
			if _, err := pc.InjectSecret("github/token", []byte("ghp_preagent")); err != nil {
				t.Fatalf("InjectSecret: %v", err)
			}
			return hookErr
		},
	})
	if !errors.Is(err, hookErr) {
		t.Fatalf("CreateWithSelection error = %v, want %v", err, hookErr)
	}
	if fb.startAgentCalls != 0 {
		t.Fatalf("StartAgent calls = %d, want 0", fb.startAgentCalls)
	}
	if fb.stopped == nil {
		t.Fatal("Stop was not called")
	}
	assertMissingDir(t, m.secrets.DirFor("sp-preagent-cleanup"))
	assertMissingDir(t, m.artifacts.DirFor("sp-preagent-cleanup"))
	if _, ok := m.store.Get("sp-preagent-cleanup"); ok {
		t.Fatal("failed pre-agent spawn was stored")
	}
}

func TestCreateWithSelectionStartAgentFailureCleanupRemovesSecretAndArtifactDirs(t *testing.T) {
	startErr := errors.New("agent failed")
	fb := &preAgentBackend{startAgentErr: startErr}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	_, err := m.CreateWithSelection(context.Background(), "sp-startagent-cleanup", writeApp(t), "model", "", "", 0, AgentSelection{
		BeforeStartAgent: func(_ context.Context, pc PreAgentContext) error {
			secretPath, err := pc.InjectSecret("github/token", []byte("ghp_preagent"))
			if err != nil {
				t.Fatalf("InjectSecret: %v", err)
			}
			fb.secretPath = secretPath
			return nil
		},
	})
	if !errors.Is(err, startErr) {
		t.Fatalf("CreateWithSelection error = %v, want %v", err, startErr)
	}
	if fb.startAgentCalls != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", fb.startAgentCalls)
	}
	if fb.stopped == nil {
		t.Fatal("Stop was not called")
	}
	assertMissingDir(t, m.secrets.DirFor("sp-startagent-cleanup"))
	assertMissingDir(t, m.artifacts.DirFor("sp-startagent-cleanup"))
	if _, ok := m.store.Get("sp-startagent-cleanup"); ok {
		t.Fatal("failed start-agent spawn was stored")
	}
}

func TestCreateWithSelectionBeforeStartAgentFailureRemovesFloor(t *testing.T) {
	hookErr := errors.New("pre-agent failed")
	fb := &preAgentBackend{}
	fa := &fakeApplier{}
	m := NewManagerWithBackend(fb, fa, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), EgressEnforce: true,
	})

	_, err := m.CreateWithSelection(context.Background(), "sp-preagent-floor", writeApp(t), "model", "", "", 0, AgentSelection{
		BeforeStartAgent: func(_ context.Context, pc PreAgentContext) error {
			if _, err := pc.InjectSecret("github/token", []byte("ghp_preagent")); err != nil {
				t.Fatalf("InjectSecret: %v", err)
			}
			return hookErr
		},
	})
	if !errors.Is(err, hookErr) {
		t.Fatalf("CreateWithSelection error = %v, want %v", err, hookErr)
	}
	if !fa.applied {
		t.Fatal("egress floor was not applied")
	}
	if !fa.removed {
		t.Fatal("egress floor was not removed after pre-agent failure")
	}
	if fb.startAgentCalls != 0 {
		t.Fatalf("StartAgent calls = %d, want 0", fb.startAgentCalls)
	}
	if fb.stopped == nil {
		t.Fatal("Stop was not called")
	}
}

type contextCheckingApplier struct {
	removed        bool
	removeCanceled bool
}

func (a *contextCheckingApplier) Apply(context.Context, []firewall.Rule) error {
	return nil
}

func (a *contextCheckingApplier) Remove(ctx context.Context, _ []firewall.Rule) error {
	a.removed = true
	if err := ctx.Err(); err != nil {
		a.removeCanceled = true
		return err
	}
	return nil
}

func TestCreateWithSelectionPreAgentFailureCleanupIgnoresCanceledCreateContext(t *testing.T) {
	hookErr := errors.New("pre-agent failed")
	fb := &preAgentBackend{}
	fa := &contextCheckingApplier{}
	m := NewManagerWithBackend(fb, fa, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), EgressEnforce: true,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := m.CreateWithSelection(ctx, "sp-preagent-canceled-cleanup", writeApp(t), "model", "", "", 0, AgentSelection{
		BeforeStartAgent: func(context.Context, PreAgentContext) error {
			cancel()
			return hookErr
		},
	})
	if !errors.Is(err, hookErr) {
		t.Fatalf("CreateWithSelection error = %v, want %v", err, hookErr)
	}
	if !fa.removed {
		t.Fatal("egress floor was not removed after pre-agent failure")
	}
	if fa.removeCanceled {
		t.Fatal("egress floor cleanup used the canceled create context")
	}
	if fb.stopped == nil {
		t.Fatal("Stop was not called")
	}
	if fb.startAgentCalls != 0 {
		t.Fatalf("StartAgent calls = %d, want 0", fb.startAgentCalls)
	}
	assertMissingDir(t, m.secrets.DirFor("sp-preagent-canceled-cleanup"))
	assertMissingDir(t, m.artifacts.DirFor("sp-preagent-canceled-cleanup"))
	if _, ok := m.store.Get("sp-preagent-canceled-cleanup"); ok {
		t.Fatal("failed pre-agent spawn was stored")
	}
}

func assertMissingDir(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir %s exists or stat failed with unexpected error: %v", dir, err)
	}
}
