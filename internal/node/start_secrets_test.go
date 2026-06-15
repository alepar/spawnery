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

type startPodCountBackend struct {
	scriptedPodBackend

	mu            sync.Mutex
	startPodCalls int
}

func (b *startPodCountBackend) StartPod(ctx context.Context, spec runtime.PodSpec) (*runtime.PodHandle, error) {
	b.mu.Lock()
	b.startPodCalls++
	b.mu.Unlock()
	return b.scriptedPodBackend.StartPod(ctx, spec)
}

func (b *startPodCountBackend) podCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.startPodCalls
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

func TestConsumeStartupSecretsRollsBackBatchOnLaterFailure(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp-start-rollback", uint64(11)
	holder := startupSecretHolder(t, nodeID)
	a := startupSecretAttacher(t, &scriptedPodBackend{}, &fakeCPStream{}, nodeID, holder, t.TempDir())
	first := sealSecret(t, holder, spawnID, gen, "legacy/first", "FIRST_TOKEN", 1, "startup-first-1", []byte("first"))
	second := sealSecret(t, holder, spawnID, gen, "", "SECOND_TOKEN", 1, "startup-second-1", []byte("second"))
	routes := map[string]startupSecretRoute{
		"FIRST_TOKEN": {target: nodev1.ArtifactTarget_ARTIFACT_TARGET_AGENT, destPath: "first/token"},
	}
	injected := map[string]string{}
	inject := func(target string, plaintext []byte) (string, error) {
		injected[target] = string(plaintext)
		return target, nil
	}

	err := a.consumeStartupSecrets(context.Background(), spawnID, gen, []*nodev1.SealedSecret{first, second}, routes, inject, "", "")
	if err == nil {
		t.Fatal("consumeStartupSecrets returned nil, want failure on second secret")
	}
	if got := injected["first/token"]; got != "first" {
		t.Fatalf("first secret inject = %q, want first", got)
	}

	delete(injected, "first/token")
	if err := a.consumeStartupSecrets(context.Background(), spawnID, gen, []*nodev1.SealedSecret{first}, routes, inject, "", ""); err != nil {
		t.Fatalf("first secret should be retryable after batch rollback: %v", err)
	}
	if got := injected["first/token"]; got != "first" {
		t.Fatalf("retry first secret inject = %q, want first", got)
	}
}

func TestConsumeStartupSecretsDoesNotConsumeBeforeBatchOpenSucceeds(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp-start-open-first", uint64(14)
	holder := startupSecretHolder(t, nodeID)
	a := startupSecretAttacher(t, &scriptedPodBackend{}, &fakeCPStream{}, nodeID, holder, t.TempDir())
	first := sealSecret(t, holder, spawnID, gen, "legacy/first", "FIRST_TOKEN", 1, "startup-open-first-1", []byte("first"))
	badSecond := &nodev1.SealedSecret{
		SecretId:   "SECOND_TOKEN",
		TargetPath: "legacy/second",
		Version:    1,
		DeliveryId: "startup-open-second-1",
		Sealed:     []byte(`{"not":"a valid node sealed payload"}`),
	}
	routes := map[string]startupSecretRoute{
		"FIRST_TOKEN": {target: nodev1.ArtifactTarget_ARTIFACT_TARGET_AGENT, destPath: "first/token"},
	}
	injectCalls := 0
	inject := func(target string, plaintext []byte) (string, error) {
		injectCalls++
		return target, nil
	}

	err := a.consumeStartupSecrets(context.Background(), spawnID, gen, []*nodev1.SealedSecret{first, badSecond}, routes, inject, "", "")
	if err == nil {
		t.Fatal("consumeStartupSecrets returned nil, want failure on bad second secret")
	}
	if injectCalls != 0 {
		t.Fatalf("inject calls before full batch open = %d, want 0", injectCalls)
	}

	if err := a.consumeStartupSecrets(context.Background(), spawnID, gen, []*nodev1.SealedSecret{first}, routes, inject, "", ""); err != nil {
		t.Fatalf("first secret should be retryable after batch open rollback: %v", err)
	}
	if injectCalls != 1 {
		t.Fatalf("inject calls after retry = %d, want 1", injectCalls)
	}
}

func TestStartSpawnRejectsDuplicateStartupSecretRoutes(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp-start-dup-route", uint64(13)
	holder := startupSecretHolder(t, nodeID)
	be := &startPodCountBackend{}
	fs := &fakeCPStream{}
	a := startupSecretAttacher(t, be, fs, nodeID, holder, t.TempDir())
	sec := sealSecret(t, holder, spawnID, gen, "github/token", "GITHUB_TOKEN", 1, "startup-dup-route-1", []byte("token"))

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: spawnID, AppRef: writeNodeApp(t), Model: "m", Generation: gen,
		Artifacts: []*nodev1.ArtifactSpec{
			{
				Id: "github-agent", Sensitive: true, EnvVarName: "GITHUB_TOKEN", DestPath: "github/token",
				TargetContainer: nodev1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
			},
			{
				Id: "github-sidecar", Sensitive: true, EnvVarName: "GITHUB_TOKEN", DestPath: "sidecar/token",
				TargetContainer: nodev1.ArtifactTarget_ARTIFACT_TARGET_SIDECAR,
			},
		},
		Secrets: []*nodev1.SealedSecret{sec},
	})

	if got := lastPhase(fs.phasesFor(spawnID)); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("final phase = %v, want ERROR", got)
	}
	if got := be.podCalls(); got != 0 {
		t.Fatalf("StartPod calls = %d, want 0", got)
	}
	if _, ok := a.mgr.Store().Get(spawnID); ok {
		t.Fatal("spawn should not be stored after duplicate startup route rejection")
	}

	_, err := startupSecretRoutesFromProto([]*nodev1.ArtifactSpec{
		{
			Id: "github-agent", Sensitive: true, EnvVarName: "GITHUB_TOKEN", DestPath: "github/token",
			TargetContainer: nodev1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
		},
		{
			Id: "github-sidecar", Sensitive: true, EnvVarName: "GITHUB_TOKEN", DestPath: "sidecar/token",
			TargetContainer: nodev1.ArtifactTarget_ARTIFACT_TARGET_SIDECAR,
		},
	})
	if err == nil {
		t.Fatal("startupSecretRoutesFromProto returned nil error, want duplicate env_var_name rejection")
	}
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

func TestStartSpawnPostsSidecarSecretRetriesUntilControlReady(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp-start-sidecar-retry", uint64(12)
	holder := startupSecretHolder(t, nodeID)
	dataRoot := t.TempDir()
	doer := &flakySidecarCredentialsDoer{failures: 1}
	be := &startAgentCheckBackend{scriptedPodBackend: scriptedPodBackend{script: scriptGoose}}
	be.check = func() {
		if got := doer.calls(); got != 2 {
			t.Fatalf("sidecar credential POST calls before StartAgent = %d, want 2", got)
		}
	}
	fs := &fakeCPStream{}
	a := startupSecretAttacher(t, be, fs, nodeID, holder, dataRoot)
	a.ctrlHTTP = doer
	sec := sealSecret(t, holder, spawnID, gen, "sidecar/key", "OPENROUTER_API_KEY", 1, "startup-sidecar-retry-1", []byte("sk-sidecar"))

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
	if be.agentCalls() != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", be.agentCalls())
	}
	if got := doer.calls(); got != 2 {
		t.Fatalf("sidecar credential POST calls = %d, want 2", got)
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
