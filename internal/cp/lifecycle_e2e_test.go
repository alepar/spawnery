//go:build e2e

package cp_test

// TestSuspendResumeLifecycleE2E validates the sp-u53.7 transition-coordination epic
// end-to-end over a real stack: real CP httptest server + real attached node/spawnlet
// with the Docker runtime + real stub-agent/sidecar pods. It guards:
//
//   - The ACTIVE→SUSPENDED→ACTIVE state-machine transitions (with journal snapshot/restore).
//   - Generation increment on resume (gen2 > gen1).
//   - The pod stays functional across the suspend/resume cycle: a session echo works both
//     before and after resume.
//   - The suspend gate/progress path (SnapshotForSuspend + FinishSuspend) produces valid
//     mount markers without stalling the CP stall detector.
//
// Requires Docker + the stub/sidecar images; skips if Garage S3 credentials are
// unavailable (run `just garage` first). FAILS (no skip) if Docker or images are broken.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"spawnery/internal/storage/journal"
)

// garageS3Config reads the Garage S3 credentials for the journaler. It tries (in order):
//  1. JOURNAL_S3_ENDPOINT + JOURNAL_S3_BUCKET + JOURNAL_S3_ACCESS_KEY + JOURNAL_S3_SECRET_KEY env vars.
//  2. The repo's deploy/garage/dev-creds.env file (written by `just garage`).
//
// Returns a populated S3Config and true when credentials are found; otherwise ("", false)
// with no credentials set — the caller FAILS the test (a down dependency is an error, not a skip).
func garageS3Config(t *testing.T) (journal.S3Config, bool) {
	t.Helper()

	// Priority 1: environment variables (from `just test-garage-env` or CI).
	if ep := os.Getenv("JOURNAL_S3_ENDPOINT"); ep != "" {
		return journal.S3Config{
			Endpoint:        ep,
			Bucket:          envOr("JOURNAL_S3_BUCKET", "spawnery-journal"),
			AccessKeyID:     os.Getenv("JOURNAL_S3_ACCESS_KEY"),
			SecretAccessKey: os.Getenv("JOURNAL_S3_SECRET_KEY"),
			Region:          envOr("JOURNAL_S3_REGION", "garage"),
			DisableTLS:      os.Getenv("JOURNAL_S3_DISABLE_TLS") == "true",
		}, true
	}

	// Priority 2: deploy/garage/dev-creds.env (repo-relative; test CWD is internal/cp).
	credsFile := filepath.Join("..", "..", "deploy", "garage", "dev-creds.env")
	data, err := os.ReadFile(credsFile)
	if err != nil {
		return journal.S3Config{}, false // Garage not started
	}

	kv := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		kv[k] = v
	}
	ep := kv["JOURNAL_S3_ENDPOINT"]
	if ep == "" {
		return journal.S3Config{}, false
	}
	disableTLS := kv["JOURNAL_S3_DISABLE_TLS"] == "true"
	return journal.S3Config{
		Endpoint:        ep,
		Bucket:          kvOr(kv, "JOURNAL_S3_BUCKET", "spawnery-journal"),
		AccessKeyID:     kv["JOURNAL_S3_ACCESS_KEY"],
		SecretAccessKey: kv["JOURNAL_S3_SECRET_KEY"],
		Region:          kvOr(kv, "JOURNAL_S3_REGION", "garage"),
		DisableTLS:      disableTLS,
	}, true
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func kvOr(m map[string]string, key, def string) string {
	if v := m[key]; v != "" {
		return v
	}
	return def
}

// buildJournalForTest wires the node-local transient-tier journaler against Garage S3.
// FAILS the test (t.Fatalf) when Garage is not available — under the e2e build tag a required
// dependency being down is an error, not a reason to silently skip (hidden breakage).
func buildJournalForTest(t *testing.T, dataRoot string) journal.JournalManager {
	t.Helper()

	s3cfg, ok := garageS3Config(t)
	if !ok {
		t.Fatalf("Garage S3 credentials not available — start the dev Garage (`just garage`), " +
			"or set JOURNAL_S3_ENDPOINT + JOURNAL_S3_BUCKET + JOURNAL_S3_ACCESS_KEY + JOURNAL_S3_SECRET_KEY")
		return nil
	}

	root := filepath.Join(dataRoot, "journal")

	backend, err := journal.NewBackend(journal.BackendConfig{
		Kind: journal.BackendS3,
		S3:   s3cfg,
	})
	if err != nil {
		t.Fatalf("journal S3 backend: %v", err)
	}

	// Node key + node-local custody (password sealed under this node's key).
	keyfile := filepath.Join(root, "node.key")
	if err := journal.GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatalf("journal node key: %v", err)
	}
	custody, err := journal.NewNodeLocalCustody(keyfile, filepath.Join(root, "sealed"))
	if err != nil {
		t.Fatalf("journal custody: %v", err)
	}

	jm, err := journal.NewManager(journal.Config{
		RepoRoot:    filepath.Join(root, "repos"),
		Backend:     backend,
		Custody:     custody,
		OwnerSealed: journal.NewOwnerSealedCustody(),
	})
	if err != nil {
		t.Fatalf("journal manager: %v", err)
	}
	return jm
}

// findSpawnGeneration returns the current generation for spawnID from ListSpawns.
func findSpawnGeneration(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, spawnID string) uint64 {
	t.Helper()
	ls, err := cl.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatalf("ListSpawns: %v", err)
	}
	for _, sp := range ls.Msg.Spawns {
		if sp.SpawnId == spawnID {
			return sp.GetGeneration()
		}
	}
	t.Fatalf("spawn %s not found in ListSpawns", spawnID)
	return 0
}

// waitForStatus polls ListSpawns until the spawn reaches one of the target statuses,
// failing fast on the given terminal statuses or after the deadline.
func waitForStatus(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, spawnID string, want cpv1.SpawnStatus, terminals ...cpv1.SpawnStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		ls, err := cl.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
		if err != nil {
			t.Fatalf("ListSpawns: %v", err)
		}
		for _, sp := range ls.Msg.Spawns {
			if sp.SpawnId != spawnID {
				continue
			}
			if sp.Status == want {
				return
			}
			for _, term := range terminals {
				if sp.Status == term {
					t.Fatalf("spawn %s reached terminal status %v before %v", spawnID, sp.Status, want)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn %s did not reach %v within 2m", spawnID, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// promptEchoOnce opens a fresh Session stream, sends one prompt, waits for the ECHO reply,
// then closes the stream. Returns the concatenated agent text.
func promptEchoOnce(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, spawnID, text string) string {
	t.Helper()

	stream := cl.Session(ctx)
	// Bind frame: attach this client to the named spawn.
	if err := stream.Send(&cpv1.Frame{SpawnId: spawnID}); err != nil {
		t.Fatalf("promptEchoOnce bind %q: %v", text, err)
	}

	// Fan received frames into a pipe so we can scan line-by-line.
	pr, pw := io.Pipe()
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			_, _ = pw.Write(f.Data)
		}
	}()

	b, _ := json.Marshal(map[string]any{"kind": "prompt", "text": text})
	if err := stream.Send(&cpv1.Frame{SpawnId: spawnID, Data: append(b, '\n')}); err != nil {
		t.Fatalf("promptEchoOnce send %q: %v", text, err)
	}

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var got strings.Builder
	for sc.Scan() {
		var fr struct {
			Kind  string `json:"kind"`
			Text  string `json:"text"`
			State string `json:"state"`
		}
		if json.Unmarshal(sc.Bytes(), &fr) != nil {
			continue
		}
		if fr.Kind == "agent" {
			got.WriteString(fr.Text)
		}
		if fr.Kind == "turn" && fr.State == "idle" {
			break
		}
	}
	if err := sc.Err(); err != nil {
		// EOF is expected when the stream closes; other errors are real.
		if err != io.EOF && !strings.Contains(err.Error(), "EOF") {
			t.Fatalf("promptEchoOnce read frames %q: %v", text, err)
		}
	}
	stream.CloseRequest()

	result := got.String()
	if !strings.Contains(result, "ECHO: "+text) {
		t.Fatalf("promptEchoOnce: want ECHO: %q, got %q", text, result)
	}
	return result
}

// TestSuspendResumeLifecycleE2E is the main end-to-end suspend/resume lifecycle test.
// It validates the full ACTIVE→SUSPENDED→ACTIVE state machine over a real Docker stack.
func TestSuspendResumeLifecycleE2E(t *testing.T) {
	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}

	// --- CP ---
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

	// Unique in-memory SQLite DSN per test so parallel e2e runs don't share state.
	st, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    "file:lifecycle_e2e_suspend_resume?mode=memory&cache=shared&_pragma=foreign_keys(1)",
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

	// --- Node with S3 journal configured ---
	dataRoot := t.TempDir()
	jm := buildJournalForTest(t, dataRoot) // skips if Garage unavailable

	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage: "spawnery/stubagent:dev", SidecarImage: "spawnery/sidecar:dev",
		OpenRouterKey: "unused", DataRoot: dataRoot,
	})
	mgr.SetJournal(jm, filepath.Join(dataRoot, "journal", "state"))

	nodeCtx, stopNode := context.WithCancel(context.Background())
	defer stopNode()
	go node.Run(nodeCtx, mgr, h2cClient(), node.Config{
		NodeID: "lc-e2e-n1", CPURL: cpSrv.URL, MaxSpawns: 2,
		AgentImage: "spawnery/stubagent:dev",
	})

	// Wait for node to register with the CP.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := reg.Get("lc-e2e-n1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node lc-e2e-n1 never registered with CP")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// --- Client ---
	cl := cpv1connect.NewSpawnServiceClient(h2cClient(), cpSrv.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer("dev-token")))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Phase 1: CreateSpawn → ACTIVE.
	t.Log("phase 1: CreateSpawn")
	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "x"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := cs.Msg.SpawnId
	defer func() {
		_ = func() error {
			_, err := cl.StopSpawn(context.Background(), connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))
			return err
		}()
	}()

	waitActive(ctx, t, cl, id)
	gen1 := findSpawnGeneration(ctx, t, cl, id)
	t.Logf("spawn %s ACTIVE at generation %d", id, gen1)

	// Phase 2: First echo — proves the pod is reachable before suspend.
	t.Log("phase 2: echo before suspend")
	got1 := promptEchoOnce(ctx, t, cl, id, "before suspend")
	t.Logf("pre-suspend echo: %q", got1)

	// Phase 2b: DATA WRITE — write a known marker file into the journaled `main` mount's
	// host directory so it is captured by the journal snapshot during suspend.
	// The scratch backend places the host dir at DataRoot/<spawnID>/<mountName>.
	// The journal watcher and SnapshotForSuspend both operate on this path, so writing
	// here (as root, from the test) is the correct and most direct injection point.
	mainMountHostDir := filepath.Join(dataRoot, id, "main")
	markerContent := "lifecycle-test-marker:" + id
	markerHostPath := filepath.Join(mainMountHostDir, "lifecycle-test-marker.txt")
	if err := os.WriteFile(markerHostPath, []byte(markerContent), 0o644); err != nil {
		t.Fatalf("write marker to %s: %v", markerHostPath, err)
	}
	t.Logf("wrote marker to host dir: %s", markerHostPath)

	// Phase 3: SuspendSpawn — blocking: returns when the node has emitted SuspendComplete
	// and the CP has written SUSPENDED to the store.
	t.Log("phase 3: SuspendSpawn (blocking)")
	if _, err := cl.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}
	t.Log("SuspendSpawn returned — spawn should be SUSPENDED")

	// Verify SUSPENDED status (SuspendSpawn is blocking but a quick check confirms the store).
	waitForStatus(ctx, t, cl, id,
		cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED,
		cpv1.SpawnStatus_SPAWN_STATUS_ERROR,
		cpv1.SpawnStatus_SPAWN_STATUS_DELETED,
	)
	t.Log("status: SUSPENDED confirmed")

	// Phase 4: ResumeSpawn — blocking: returns when the node has provisioned a new pod
	// and the CP has written ACTIVE at gen+1.
	t.Log("phase 4: ResumeSpawn (blocking)")
	if _, err := cl.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("ResumeSpawn: %v", err)
	}
	t.Log("ResumeSpawn returned — spawn should be ACTIVE")

	// Phase 5: Assert generation incremented and status is ACTIVE.
	gen2 := findSpawnGeneration(ctx, t, cl, id)
	t.Logf("spawn %s ACTIVE at generation %d (was %d)", id, gen2, gen1)
	if gen2 <= gen1 {
		t.Fatalf("generation did not increment on resume: gen1=%d gen2=%d", gen1, gen2)
	}

	waitForStatus(ctx, t, cl, id,
		cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE,
		cpv1.SpawnStatus_SPAWN_STATUS_ERROR,
		cpv1.SpawnStatus_SPAWN_STATUS_DELETED,
	)

	// Phase 6: Second echo — proves the pod works after resume.
	// The journal restore should have brought the "main" mount data back.
	t.Log("phase 6: echo after resume")
	got2 := promptEchoOnce(ctx, t, cl, id, "after resume")
	t.Logf("post-resume echo: %q", got2)

	// Phase 6b: DATA SURVIVAL — assert the marker written before suspend is present in
	// the restored `main` mount. The journal Restore unpacked the snapshot back into the
	// same host path (DataRoot/<spawnID>/main) on resume.
	t.Log("phase 6b: data survival check")
	restored, err := os.ReadFile(markerHostPath)
	if err != nil {
		t.Fatalf("data survival FAIL: marker file %s not found after resume: %v", markerHostPath, err)
	}
	if string(restored) != markerContent {
		t.Fatalf("data survival FAIL: marker content mismatch: want %q got %q", markerContent, string(restored))
	}
	t.Logf("data survival PASS: marker restored correctly: %q", string(restored))

	t.Logf("TestSuspendResumeLifecycleE2E PASS: gen1=%d gen2=%d", gen1, gen2)
}
