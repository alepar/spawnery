//go:build e2e

package cp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/cp"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/node"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

func TestJournalRestoredAgentCreatedFileRemainsWritableE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}
	if err := rt.Ping(ctx); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}
	remapBase := requireUsernsRemap(ctx, t, rt)

	reg := registry.New()
	rtr := router.New()
	sched := scheduler.New(reg, rtr, 60*time.Second)
	telPath := filepath.Join(t.TempDir(), "events.jsonl")
	tel, err := telemetry.NewJSONLSink(telPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tel.Close()

	authn := auth.NewVerifier(auth.VerifierConfig{DevTokens: map[string]string{"dev-token": "alice"}, DevMode: true})
	appRef, err := filepath.Abs("../../examples/secret-app")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    "file:datafs_perms_e2e?mode=memory&cache=shared&_pragma=foreign_keys(1)",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := cp.Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]cp.AppSeed{{ID: "secret-app", Ref: appRef, Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}

	srv := cp.NewServer(reg, rtr, sched, st, tel)
	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(srv))
	mux.Handle(cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(authn.Interceptor())))
	cpSrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	cpSrv.Start()
	defer cpSrv.Close()

	dataRoot := repoLocalDataRoot(t)
	jm := buildJournalForTest(t, dataRoot)
	const nodeID = "datafs-perms-e2e-n1"
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage:       "spawnery/stubagent:dev",
		SidecarImage:     "spawnery/sidecar:dev",
		OpenRouterKey:    "unused",
		DataRoot:         dataRoot,
		NodeID:           nodeID,
		UsernsMode:       "remap",
		UsernsRemapBase:  remapBase,
		DeltaCapture:     true,
		EgressEnforce:    false,
		DeltaSquashDepth: 16,
	})
	mgr.SetJournal(jm, filepath.Join(dataRoot, "journal", "state"))

	nodeCtx, stopNode := context.WithCancel(context.Background())
	defer stopNode()
	go node.Run(nodeCtx, mgr, h2cClient(), node.Config{
		NodeID:     nodeID,
		CPURL:      cpSrv.URL,
		MaxSpawns:  2,
		AgentImage: "spawnery/stubagent:dev",
	})

	waitNodeRegistered(t, reg, nodeID)

	cl := cpv1connect.NewSpawnServiceClient(h2cClient(), cpSrv.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer("dev-token")))

	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "x"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := cs.Msg.SpawnId
	defer func() {
		_, _ = cl.StopSpawn(context.Background(), connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))
	}()

	waitActive(ctx, t, cl, id)
	gen1 := findSpawnGeneration(ctx, t, cl, id)
	agent1 := findAgentContainer(ctx, t, id, gen1)

	assertContainerOwner(ctx, t, agent1, "/app", "0:0")
	assertContainerOwner(ctx, t, agent1, "/app/AGENTS.md", "0:0")
	assertContainerOwner(ctx, t, agent1, "/app/data", "0:0")
	assertContainerOwner(ctx, t, agent1, "/app/data/README.md", "0:0")

	// Keep the file agent-owned and non-world-writable, while leaving read access
	// for the rootless journal process so the e2e proves restore permissions
	// instead of snapshot readability.
	createCmd := "printf survive > /app/data/agentfile && chmod 604 /app/data/agentfile"
	runDocker(ctx, t, "exec", agent1, "sh", "-c", createCmd)

	if _, err := cl.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}
	waitForStatus(ctx, t, cl, id,
		cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED,
		cpv1.SpawnStatus_SPAWN_STATUS_ERROR,
		cpv1.SpawnStatus_SPAWN_STATUS_DELETED,
	)

	if _, err := cl.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("ResumeSpawn: %v", err)
	}
	waitActive(ctx, t, cl, id)
	gen2 := findSpawnGeneration(ctx, t, cl, id)
	if gen2 <= gen1 {
		t.Fatalf("generation did not increment on resume: gen1=%d gen2=%d", gen1, gen2)
	}

	agent2 := findAgentContainer(ctx, t, id, gen2)
	assertContainerOwner(ctx, t, agent2, "/app", "0:0")
	assertContainerOwner(ctx, t, agent2, "/app/AGENTS.md", "0:0")
	assertContainerOwner(ctx, t, agent2, "/app/data", "0:0")
	assertContainerOwner(ctx, t, agent2, "/app/data/agentfile", "0:0")
	if mode := dockerOutput(ctx, t, "exec", agent2, "stat", "-c", "%a", "/app/data/agentfile"); mode != "666" {
		t.Fatalf("restored agentfile mode = %s, want 666", mode)
	}
	runDocker(ctx, t, "exec", agent2, "sh", "-c", "printf more >> /app/data/agentfile")
	got := dockerOutput(ctx, t, "exec", agent2, "cat", "/app/data/agentfile")
	if got != "survivemore" {
		t.Fatalf("restored agentfile content = %q, want %q", got, "survivemore")
	}
}

func assertContainerOwner(ctx context.Context, t *testing.T, containerID, path, want string) {
	t.Helper()
	if got := dockerOutput(ctx, t, "exec", containerID, "stat", "-c", "%u:%g", path); got != want {
		t.Fatalf("%s owner = %s, want %s", path, got, want)
	}
}

func requireUsernsRemap(ctx context.Context, t *testing.T, rt *runtime.Docker) uint32 {
	t.Helper()
	base, active, err := rt.UsernsRemap(ctx)
	if err != nil {
		t.Fatalf("probe docker userns-remap: %v", err)
	}
	if !active {
		t.Fatal("Docker userns-remap is required for this e2e test; enable USERNS_MODE=remap host setup")
	}
	return base
}

func repoLocalDataRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".spawns"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir e2e data root parent: %v", err)
	}
	dataRoot, err := os.MkdirTemp(root, "datafs-perms-e2e-")
	if err != nil {
		t.Fatalf("mkdir e2e data root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataRoot) })
	return dataRoot
}

func waitNodeRegistered(t *testing.T, reg *registry.Registry, nodeID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := reg.Get(nodeID); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %s never registered with CP", nodeID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func findAgentContainer(ctx context.Context, t *testing.T, spawnID string, generation uint64) string {
	t.Helper()
	out := dockerOutput(ctx, t,
		"ps",
		"--filter", "label="+runtime.LabelSpawnID+"="+spawnID,
		"--filter", "label="+runtime.LabelRole+"=agent",
		"--filter", "label="+runtime.LabelGeneration+"="+strconv.FormatUint(generation, 10),
		"-q",
	)
	ids := strings.Fields(out)
	if len(ids) != 1 {
		t.Fatalf("docker ps found %d agent containers for spawn %s generation %d: %q", len(ids), spawnID, generation, out)
	}
	return ids[0]
}

func runDocker(ctx context.Context, t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func dockerOutput(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
