package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime"
	"spawnery/internal/secrets/subkey"
	"spawnery/internal/spawnlet"
)

type startAgentCheckBackend struct {
	scriptedPodBackend
	check func()

	mu              sync.Mutex
	startAgentCalls int
}

func (b *startAgentCheckBackend) StartAgent(ctx context.Context, h *runtime.PodHandle, spec runtime.AgentSpec) error {
	if b.check != nil {
		b.check()
	}
	b.mu.Lock()
	b.startAgentCalls++
	b.mu.Unlock()
	return b.scriptedPodBackend.StartAgent(ctx, h, spec)
}

func (b *startAgentCheckBackend) agentCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.startAgentCalls
}

func startupSecretAttacher(t *testing.T, be runtime.PodBackend, fs *fakeCPStream, nodeID string, holder *subkey.Node, dataRoot string) *attacher {
	t.Helper()
	mgr := spawnlet.NewManagerWithBackend(be, noopApplier{}, spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot,
	})
	a := newAttacher(mgr, fs)
	a.cfg.NodeID = nodeID
	a.cfg.SubKeys = holder
	return a
}

func startupSecretHolder(t *testing.T, nodeID string) *subkey.Node {
	t.Helper()
	holder := subkey.NewNode(testSignKey(t), nodeID, 0)
	if _, err := holder.Rotate(time.Now()); err != nil {
		t.Fatal(err)
	}
	return holder
}

func TestStartSpawnInjectsAgentSecretBeforeStartAgent(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp-start-agent", uint64(7)
	holder := startupSecretHolder(t, nodeID)
	dataRoot := t.TempDir()
	secretPath := filepath.Join(dataRoot, "secrets", spawnID, "github", "token")
	be := &startAgentCheckBackend{scriptedPodBackend: scriptedPodBackend{script: scriptGoose}}
	be.check = func() {
		got, err := os.ReadFile(secretPath)
		if err != nil {
			t.Fatalf("StartAgent did not see staged secret at %s: %v", secretPath, err)
		}
		if string(got) != "ghp_startup_token" {
			t.Fatalf("secret content = %q, want startup token", got)
		}
	}
	fs := &fakeCPStream{}
	a := startupSecretAttacher(t, be, fs, nodeID, holder, dataRoot)
	sec := sealSecret(t, holder, spawnID, gen, "legacy/path", "GITHUB_TOKEN", 1, "startup-agent-1", []byte("ghp_startup_token"))

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: spawnID, AppRef: writeNodeApp(t), Model: "m", Generation: gen,
		Artifacts: []*nodev1.ArtifactSpec{{
			Id: "github-token", Sensitive: true, EnvVarName: "GITHUB_TOKEN", DestPath: "github/token",
			TargetContainer: nodev1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
		}},
		Secrets: []*nodev1.SealedSecret{sec},
	})
	defer a.stopSpawn(context.Background(), spawnID)

	if got := lastPhase(fs.phasesFor(spawnID)); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("final phase = %v, want ACTIVE", got)
	}
	if be.agentCalls() != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", be.agentCalls())
	}
}

func TestStartSpawnPostsSidecarSecretBeforeStartAgent(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp-start-sidecar", uint64(8)
	holder := startupSecretHolder(t, nodeID)
	dataRoot := t.TempDir()
	doer := stubDoerOK()
	be := &startAgentCheckBackend{scriptedPodBackend: scriptedPodBackend{script: scriptGoose}}
	be.check = func() {
		if doer.calls != 1 {
			t.Fatalf("sidecar credential POST calls before StartAgent = %d, want 1", doer.calls)
		}
		if got := doer.gotReq.URL.String(); got != "http://10.0.0.5:8081/control/credentials" {
			t.Fatalf("credential URL = %s", got)
		}
		if got := doer.gotReq.Header.Get("Authorization"); got == "" || got == "Bearer " {
			t.Fatalf("authorization = %q, want bearer control token", got)
		}
		var body struct {
			Key      string `json:"key"`
			Upstream string `json:"upstream"`
		}
		if err := json.Unmarshal([]byte(doer.gotBody), &body); err != nil {
			t.Fatalf("credential POST body not JSON: %v", err)
		}
		if body.Key != "sk-sidecar" || body.Upstream != "" {
			t.Fatalf("credential body = %+v, want key and empty upstream", body)
		}
		if _, err := os.Stat(filepath.Join(dataRoot, "secrets", spawnID, "sidecar", "key")); !os.IsNotExist(err) {
			t.Fatalf("sidecar credential must not be written to agent secrets dir, stat err=%v", err)
		}
	}
	fs := &fakeCPStream{}
	a := startupSecretAttacher(t, be, fs, nodeID, holder, dataRoot)
	a.ctrlHTTP = doer
	sec := sealSecret(t, holder, spawnID, gen, "sidecar/key", "OPENROUTER_API_KEY", 1, "startup-sidecar-1", []byte("sk-sidecar"))

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: spawnID, AppRef: writeNodeApp(t), Model: "m", Generation: gen,
		Artifacts: []*nodev1.ArtifactSpec{{
			Id: "byok", Sensitive: true, EnvVarName: "OPENROUTER_API_KEY",
			TargetContainer: nodev1.ArtifactTarget_ARTIFACT_TARGET_SIDECAR,
		}},
		Secrets: []*nodev1.SealedSecret{sec},
	})
	defer a.stopSpawn(context.Background(), spawnID)

	if got := lastPhase(fs.phasesFor(spawnID)); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("final phase = %v, want ACTIVE", got)
	}
}

func TestStartSpawnSecretFailureStopsBeforeAgent(t *testing.T) {
	const nodeID, spawnID = "node-1", "sp-start-fail"
	holder := startupSecretHolder(t, nodeID)
	dataRoot := t.TempDir()
	be := &startAgentCheckBackend{scriptedPodBackend: scriptedPodBackend{script: scriptGoose}}
	fs := &fakeCPStream{}
	a := startupSecretAttacher(t, be, fs, nodeID, holder, dataRoot)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: spawnID, AppRef: writeNodeApp(t), Model: "m", Generation: 9,
		Secrets: []*nodev1.SealedSecret{{
			SecretId: "bad", TargetPath: "bad/path", Version: 1, DeliveryId: "startup-bad-1",
			Sealed: []byte(`{"not":"a valid node sealed payload"}`),
		}},
	})

	if got := lastPhase(fs.phasesFor(spawnID)); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("final phase = %v, want ERROR", got)
	}
	if be.agentCalls() != 0 {
		t.Fatalf("StartAgent calls = %d, want 0", be.agentCalls())
	}
	if !be.wasStopped() {
		t.Fatal("startup secret failure must stop the sidecar pod")
	}
	if _, ok := a.mgr.Store().Get(spawnID); ok {
		t.Fatal("spawn should not be stored after startup secret failure")
	}
}
