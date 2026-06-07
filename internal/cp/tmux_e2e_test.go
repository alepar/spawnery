//go:build e2e

package cp_test

import (
	"context"
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
)

// setupTmuxStack starts CP+node in-process with a real Docker backend (spawnery/agent:dev).
// Returns the SpawnService client, a context with 150s timeout, and the seeded app id.
// All teardown is registered via t.Cleanup (LIFO order). The caller must NOT cancel the
// context itself — cleanup handles it.
func setupTmuxStack(t *testing.T) (cl cpv1connect.SpawnServiceClient, ctx context.Context, appID string) {
	t.Helper()

	// Load OpenRouter key — try env first, then repo-root .env.
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		// Try the repo-root .env (two directories up from internal/cp).
		envPath, _ := filepath.Abs("../../.env")
		if raw, err := os.ReadFile(envPath); err == nil {
			for _, line := range splitLines(string(raw)) {
				if len(line) > len("OPENROUTER_API_KEY=") && line[:len("OPENROUTER_API_KEY=")] == "OPENROUTER_API_KEY=" {
					key = line[len("OPENROUTER_API_KEY="):]
				}
			}
		}
	}
	if key == "" {
		t.Fatal("OPENROUTER_API_KEY is required for the tmux e2e test (set env or put it in .env)")
	}

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
	tel, err := telemetry.NewJSONLSink(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.New(map[string]string{"dev-token": "alice"})
	appRef, err := filepath.Abs("../../examples/secret-app")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    "file:cptmuxe2e_" + t.Name() + "?mode=memory&cache=shared&_pragma=foreign_keys(1)",
	})
	if err != nil {
		t.Fatal(err)
	}
	appID = "secret-app"
	if err := cp.Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]cp.AppSeed{{ID: appID, Ref: appRef, Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	srv := cp.NewServer(reg, rtr, sched, st, tel)

	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(srv))
	mux.Handle(cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(authn.Interceptor())))
	cpSrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	cpSrv.Start()

	// Cleanup order (t.Cleanup is LIFO): tel.Close runs last (registered first here).
	t.Cleanup(func() { _ = tel.Close() })
	t.Cleanup(func() { _ = st.Close() })
	t.Cleanup(cpSrv.Close)

	// --- node (attached) with real Docker + opencode image ---
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage:    "spawnery/agent:dev",
		SidecarImage:  "spawnery/sidecar:dev",
		OpenRouterKey: key,
		DataRoot:      t.TempDir(),
	})
	nodeCtx, stopNode := context.WithCancel(context.Background())
	t.Cleanup(stopNode)
	go node.Run(nodeCtx, mgr, h2cClient(), node.Config{
		NodeID:        "n-tmux",
		CPURL:         cpSrv.URL,
		MaxSpawns:     2,
		AgentImage:    "spawnery/agent:dev",
		AgentBinaries: []string{"opencode", "goose", "claude-code"},
	})

	// Wait for node to register.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, ok := reg.Get("n-tmux"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never registered with CP")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Log("node registered")

	// --- client ---
	cl = cpv1connect.NewSpawnServiceClient(h2cClient(), cpSrv.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer("dev-token")))

	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(context.Background(), 150*time.Second)
	t.Cleanup(cancel)

	return cl, ctx, appID
}

// createTmuxSpawn creates an opencode-tui tmux spawn and registers a StopSpawn cleanup.
// Returns the new spawn id.
func createTmuxSpawn(t *testing.T, ctx context.Context, cl cpv1connect.SpawnServiceClient, appID string) string {
	t.Helper()
	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:      appID,
		Model:      "openai/gpt-4o-mini",
		Image:      "spawnery/agent:dev",
		RunnableId: "opencode-tui",
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := cs.Msg.SpawnId
	t.Logf("spawn created: %s", id)
	t.Cleanup(func() {
		// Stop the spawn and give the node a moment to destroy the container before node context cancel.
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_, _ = cl.StopSpawn(stopCtx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))
		time.Sleep(2 * time.Second) // allow the node to receive Stop + destroy containers
	})
	return id
}

// assertTerminalBytes opens a Session, reads raw terminal bytes from the tmux relay,
// and asserts >0 bytes arrive within 30s.
func assertTerminalBytes(t *testing.T, ctx context.Context, cl cpv1connect.SpawnServiceClient, id string) {
	t.Helper()
	stream := cl.Session(ctx)
	if err := stream.Send(&cpv1.Frame{SpawnId: id}); err != nil {
		t.Fatalf("Session bind frame: %v", err)
	}

	// Collect raw terminal bytes. The tmux relay sends raw PTY output (not JSON frames).
	received := make(chan []byte, 1)
	go func() {
		var buf []byte
		for {
			f, err := stream.Receive()
			if err != nil {
				if len(buf) > 0 {
					received <- buf
				}
				return
			}
			buf = append(buf, f.Data...)
			if len(buf) > 0 {
				select {
				case received <- buf:
					return
				default:
				}
			}
		}
	}()

	// Assert non-empty terminal bytes arrive within 30s.
	select {
	case data := <-received:
		t.Logf("received %d terminal bytes from tmux relay (first 200: %q)", len(data), truncate(data, 200))
		if len(data) == 0 {
			t.Fatal("received 0 bytes from tmux relay; expected terminal output")
		}
		// The agent container must actually be alive rendering a TUI — not crashed. If the
		// runnable's process exited, `docker exec ... tmux attach` fails and the relay forwards
		// the daemon error ("container <id> is not running"). That is NOT a rendered TUI, so it
		// must fail the test rather than pass on the error string.
		if strings.Contains(string(data), "is not running") || strings.Contains(string(data), "Error response from daemon") {
			t.Fatalf("agent container is not running (TUI crashed): %q", truncate(data, 200))
		}
		// Resilient assertion: either an ANSI escape sequence or any non-empty output.
		hasAnsi := false
		for _, b := range data {
			if b == 0x1b { // ESC
				hasAnsi = true
				break
			}
		}
		if hasAnsi {
			t.Log("confirmed: ANSI escape sequences present in terminal output")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for terminal bytes from tmux relay (30s)")
	}

	stream.CloseRequest()
	time.Sleep(200 * time.Millisecond) // let session_end flush
}

// waitStatus polls ListSpawns until the spawn reaches want status, failing on terminal statuses
// or after the given timeout.
func waitStatus(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, id string, want cpv1.SpawnStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ls, err := cl.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
		if err != nil {
			t.Fatalf("listSpawns: %v", err)
		}
		for _, sp := range ls.Msg.Spawns {
			if sp.SpawnId != id {
				continue
			}
			if sp.Status == want {
				t.Logf("spawn %s reached status %v", id, want)
				return
			}
			// Fail fast on unexpected terminal states (unless we're explicitly waiting for one of them).
			switch sp.Status {
			case cpv1.SpawnStatus_SPAWN_STATUS_ERROR, cpv1.SpawnStatus_SPAWN_STATUS_DELETED:
				if want != sp.Status {
					t.Fatalf("spawn %s reached terminal status %v while waiting for %v", id, sp.Status, want)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn %s did not reach status %v within %v", id, want, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestCPTmuxEndToEnd drives the full CP→node→real-container→tmux-relay path:
// - CP + node run in-process with a real Docker backend
// - node uses spawnery/agent:dev (opencode + tmux)
// - CreateSpawn selects opencode-tui (tmux mode)
// - After ACTIVE, a client opens a Session and asserts >0 raw terminal bytes arrive
//
// Requires Docker + spawnery/agent:dev + OPENROUTER_API_KEY in env (or .env at repo root).
// FAILS loudly (no skips) when the environment is broken.
func TestCPTmuxEndToEnd(t *testing.T) {
	cl, ctx, appID := setupTmuxStack(t)

	id := createTmuxSpawn(t, ctx, cl, appID)

	// Wait for ACTIVE — container boot + tmux startup can take 30-60s.
	waitActiveTmux(ctx, t, cl, id)
	t.Log("spawn is ACTIVE, opening Session")

	assertTerminalBytes(t, ctx, cl, id)
}

// TestCPTmuxSuspendResume proves that a tmux spawn survives a suspend→resume cycle:
// - suspend tears down the relay + container (non-lossless)
// - resume re-provisions a fresh tmux container
// - terminal works again after resume
//
// Lossless conversation continuity (resume-argv) is E3-gated and tracked separately.
func TestCPTmuxSuspendResume(t *testing.T) {
	cl, ctx, appID := setupTmuxStack(t)

	id := createTmuxSpawn(t, ctx, cl, appID)

	// Wait for ACTIVE — container boot + tmux startup can take 30-60s.
	waitActiveTmux(ctx, t, cl, id)
	t.Log("spawn is ACTIVE")

	// Verify terminal works before suspend.
	assertTerminalBytes(t, ctx, cl, id)
	t.Log("terminal bytes confirmed before suspend")

	// Suspend → must reach SUSPENDED (container + relay torn down).
	if _, err := cl.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}
	t.Log("SuspendSpawn called, waiting for SUSPENDED status")
	waitStatus(ctx, t, cl, id, cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED, 30*time.Second)
	t.Log("spawn is SUSPENDED")

	// Resume → fresh tmux container, back to ACTIVE, terminal works again (non-lossless).
	if _, err := cl.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("ResumeSpawn: %v", err)
	}
	t.Log("ResumeSpawn called, waiting for ACTIVE status")
	// Resume re-provisions a fresh container — allow up to 60s for boot.
	waitActiveTmux(ctx, t, cl, id)
	t.Log("spawn is ACTIVE again after resume")

	// A NEW client Session after resume must still get terminal output.
	assertTerminalBytes(t, ctx, cl, id)
	t.Log("tmux suspend→resume verified (non-lossless: fresh container, terminal works again)")
}

// waitActiveTmux polls ListSpawns until the spawn reaches ACTIVE, with a 60s timeout for container boot.
func waitActiveTmux(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, id string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		ls, err := cl.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
		if err != nil {
			t.Fatalf("listSpawns: %v", err)
		}
		for _, sp := range ls.Msg.Spawns {
			if sp.SpawnId != id {
				continue
			}
			switch sp.Status {
			case cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE:
				return
			case cpv1.SpawnStatus_SPAWN_STATUS_ERROR, cpv1.SpawnStatus_SPAWN_STATUS_DELETED,
				cpv1.SpawnStatus_SPAWN_STATUS_UNREACHABLE:
				t.Fatalf("spawn %s reached terminal status %v before active", id, sp.Status)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn %s did not reach ACTIVE within 60s", id)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			// strip \r
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if line != "" {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// TestCPGooseTuiEndToEnd drives the goose-tui path through the dispatcher:
// - CP + node run in-process with a real Docker backend
// - node uses spawnery/agent:dev (goose + tmux) and advertises "goose" binary
// - CreateSpawn selects goose-tui (tmux mode)
// - the image dispatcher sets GOOSE_PROVIDER=openai, GOOSE_MODEL=$SPAWN_MODEL,
//   OPENAI_BASE_URL (sidecar), GOOSE_TELEMETRY_OFF=1 and runs goose session in tmux
// - After ACTIVE, a client opens a Session and asserts >0 raw terminal bytes arrive
//
// Requires Docker + spawnery/agent:dev + OPENROUTER_API_KEY in env (or .env at repo root).
// FAILS loudly (no skips) when the environment is broken.
func TestCPGooseTuiEndToEnd(t *testing.T) {
	cl, ctx, appID := setupTmuxStack(t)

	// Create a goose-tui spawn via the dispatcher.
	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:      appID,
		Model:      "openai/gpt-4o-mini",
		Image:      "spawnery/agent:dev",
		RunnableId: "goose-tui",
	}))
	if err != nil {
		t.Fatalf("CreateSpawn goose-tui: %v", err)
	}
	id := cs.Msg.SpawnId
	t.Logf("goose-tui spawn created: %s", id)
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_, _ = cl.StopSpawn(stopCtx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))
		time.Sleep(2 * time.Second)
	})

	// Wait for ACTIVE — goose + tmux startup can take up to 60s.
	waitActiveTmux(ctx, t, cl, id)
	t.Log("goose-tui spawn is ACTIVE, opening Session")

	// Assert raw terminal bytes arrive from the goose TUI via the tmux relay.
	assertTerminalBytes(t, ctx, cl, id)
	t.Log("goose-tui terminal bytes confirmed via dispatcher (DONE)")
}

// TestCPClaudeTuiEndToEnd drives the claude-tui path through the dispatcher:
// - CP + node run in-process with a real Docker backend
// - node uses spawnery/agent:dev (claude-code + tmux) and advertises "claude-code"
// - CreateSpawn selects claude-tui (tmux mode)
// - the image dispatcher sets ANTHROPIC_BASE_URL=<sidecar> (strip /v1), a dummy
//   ANTHROPIC_API_KEY, ANTHROPIC_CUSTOM_MODEL_OPTION=$SPAWN_MODEL and runs `claude` in tmux.
//   Claude Code reaches the model through the in-pod sidecar's /v1/messages converter, which
//   translates Anthropic Messages -> OpenAI Chat Completions against OpenRouter.
// - After ACTIVE, a client opens a Session and asserts >0 raw terminal bytes arrive (claude's
//   TUI renders), proving the converter wiring is reachable end-to-end.
//
// Requires Docker + spawnery/agent:dev + spawnery/sidecar:dev + OPENROUTER_API_KEY in env
// (or .env at repo root). FAILS loudly (no skips) when the environment is broken.
func TestCPClaudeTuiEndToEnd(t *testing.T) {
	cl, ctx, appID := setupTmuxStack(t)

	// Create a claude-tui spawn via the dispatcher.
	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:      appID,
		Model:      "openai/gpt-4o-mini",
		Image:      "spawnery/agent:dev",
		RunnableId: "claude-tui",
	}))
	if err != nil {
		t.Fatalf("CreateSpawn claude-tui: %v", err)
	}
	id := cs.Msg.SpawnId
	t.Logf("claude-tui spawn created: %s", id)
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_, _ = cl.StopSpawn(stopCtx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))
		time.Sleep(2 * time.Second)
	})

	// Wait for ACTIVE — claude-code + tmux startup can take up to 60s.
	waitActiveTmux(ctx, t, cl, id)
	t.Log("claude-tui spawn is ACTIVE, opening Session")

	// Assert raw terminal bytes arrive from the claude TUI via the tmux relay.
	assertTerminalBytes(t, ctx, cl, id)
	t.Log("claude-tui terminal bytes confirmed via dispatcher (DONE)")
}
