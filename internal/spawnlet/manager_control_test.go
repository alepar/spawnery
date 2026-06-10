package spawnlet

import (
	"context"
	"strings"
	"testing"
)

// envVal returns the value of "KEY=..." in env, and whether the key was present.
func envVal(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix), true
		}
	}
	return "", false
}

func TestCreateWiresControlTokenAndAddr(t *testing.T) {
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	sp, err := m.Create(context.Background(), "spz", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if sp.ControlToken == "" {
		t.Fatal("ControlToken is empty; want a generated per-pod secret")
	}

	// SidecarEnv must carry both control vars, with the token matching the Spawn record.
	tok, ok := envVal(fb.podSpec.SidecarEnv, "SIDECAR_CONTROL_TOKEN")
	if !ok {
		t.Fatal("SIDECAR_CONTROL_TOKEN not in SidecarEnv")
	}
	if tok != sp.ControlToken {
		t.Fatalf("env token %q != Spawn.ControlToken %q", tok, sp.ControlToken)
	}

	addr, ok := envVal(fb.podSpec.SidecarEnv, "SIDECAR_CONTROL_ADDR")
	if !ok {
		t.Fatal("SIDECAR_CONTROL_ADDR not in SidecarEnv")
	}
	// Default SidecarPort 8080 -> control port 8081, bound on all interfaces (pod IP unknown at env-build time).
	if addr != "0.0.0.0:8081" {
		t.Fatalf("SIDECAR_CONTROL_ADDR = %q, want 0.0.0.0:8081", addr)
	}

	// The stored control URL targets the pod IP + control port so the node can reach the sidecar.
	if sp.ControlURL != "http://10.0.0.5:8081/control/model" {
		t.Fatalf("ControlURL = %q, want http://10.0.0.5:8081/control/model", sp.ControlURL)
	}
}

func TestCreateControlTokenUniquePerSpawn(t *testing.T) {
	m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	sp1, err := m.Create(context.Background(), "sp1", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create sp1: %v", err)
	}
	sp2, err := m.Create(context.Background(), "sp2", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create sp2: %v", err)
	}
	if sp1.ControlToken == sp2.ControlToken {
		t.Fatalf("control tokens not unique: both = %q", sp1.ControlToken)
	}
}
