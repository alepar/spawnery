package spawnlet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/runtime"
)

type preAgentBackend struct {
	fakePodBackend
	startAgentCalls int
	secretPath      string
	secretPlaintext []byte
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
