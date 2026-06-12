//go:build e2e

package cp_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
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
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/node"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// TestCPEndToEndStub drives the whole mediation path: a node attaches to the CP,
// a client CreateSpawns + Sessions through the CP, the stub agent echoes, and
// telemetry records spawn_create -> session_start -> session_end. Requires Docker
// + the stub/sidecar images; FAILS (no skip) if the env is broken.
func TestCPEndToEndStub(t *testing.T) {
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
	// CWD is internal/cp; the node resolves the app ref against its process CWD,
	// so hand it an absolute path to the repo-root fixture.
	appRef, err := filepath.Abs("../../examples/secret-app")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: "file:cpe2e?mode=memory&cache=shared&_pragma=foreign_keys(1)"})
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

	// --- node (attached) ---
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage: "spawnery/stubagent:dev", SidecarImage: "spawnery/sidecar:dev",
		OpenRouterKey: "unused", DataRoot: t.TempDir(),
	})
	nodeCtx, stopNode := context.WithCancel(context.Background())
	defer stopNode()
	go node.Run(nodeCtx, mgr, h2cClient(), node.Config{
		NodeID: "n1", CPURL: cpSrv.URL, MaxSpawns: 2, AgentImage: "spawnery/stubagent:dev",
	})
	// wait for the node to register
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := reg.Get("n1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never registered with CP")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// --- client ---
	cl := cpv1connect.NewSpawnServiceClient(h2cClient(), cpSrv.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer("dev-token")))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "x"}))
	if err != nil {
		t.Fatalf("createSpawn: %v", err)
	}
	id := cs.Msg.SpawnId
	defer cl.StopSpawn(context.Background(), connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))

	// CreateSpawn is async (returns in 'starting'); the CP only binds the spawn to its node once the
	// node reports ACTIVE. Mirror the real client, which gates on the spawn's status going active
	// before opening a session — otherwise the attach races provisioning and gets "unknown spawn".
	waitActive(ctx, t, cl, id)

	stream := cl.Session(ctx)
	if err := stream.Send(&cpv1.Frame{SpawnId: id}); err != nil { // bind frame
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			pw.Write(f.Data)
		}
	}()

	// The CP pump speaks the frame protocol, not raw ACP: the node already performed the ACP
	// initialize + session/new, so a client just sends {"kind":"prompt"} frames and receives
	// user/agent/turn frames back (cursor-0 replay). Drive it the way the web client does.
	sendFrame := func(f map[string]any) {
		b, _ := json.Marshal(f)
		if err := stream.Send(&cpv1.Frame{SpawnId: id, Data: append(b, '\n')}); err != nil {
			t.Fatalf("send frame: %v", err)
		}
	}
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	// promptEcho drives one prompt->echo turn over the shared stream/scanner, returning the
	// concatenated agent text of the turn.
	promptEcho := func(text string) string {
		sendFrame(map[string]any{"kind": "prompt", "text": text})
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
				break // turn complete
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("read frames: %v", err)
		}
		return got.String()
	}
	if got := promptEcho("say hi"); !strings.Contains(got, "ECHO: say hi") {
		t.Fatalf("want ECHO, got %q", got)
	}

	// Regression (sp-gzvo): the spawn must SURVIVE heartbeat inventory reconciliation. The node
	// heartbeats every 5s; a generation mismatch between the live container row and the node's
	// report makes the orphan arm Stop the pod the CP itself just started. Sit through more than
	// one full heartbeat cycle, assert the spawn stays ACTIVE, then prove the pod is still live
	// end-to-end with a second echo turn.
	stillActive := time.Now().Add(7 * time.Second)
	for time.Now().Before(stillActive) {
		ls, err := cl.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
		if err != nil {
			t.Fatalf("listSpawns: %v", err)
		}
		for _, sp := range ls.Msg.Spawns {
			if sp.SpawnId == id && sp.Status != cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE {
				t.Fatalf("spawn left ACTIVE during heartbeat reconcile window: %v", sp.Status)
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if got := promptEcho("still there"); !strings.Contains(got, "ECHO: still there") {
		t.Fatalf("pod dead after heartbeat reconcile window: want ECHO, got %q", got)
	}

	stream.CloseRequest()
	time.Sleep(500 * time.Millisecond) // let session_end + StopSpawn flush
	assertTelemetry(t, telPath, id)
}

// waitActive polls ListSpawns until the spawn reaches ACTIVE (router-bound), failing fast on a
// terminal status or after 30s. CreateSpawn returns in 'starting' and provisions asynchronously.
func waitActive(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
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
			t.Fatalf("spawn %s did not reach ACTIVE within 30s", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func assertTelemetry(t *testing.T, path, spawnID string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev telemetry.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		kinds[ev.Kind] = true
		if strings.Contains(line, "ECHO") || strings.Contains(line, "say hi") {
			t.Fatalf("telemetry leaked content: %s", line)
		}
	}
	for _, k := range []string{"spawn_create", "session_start", "session_end"} {
		if !kinds[k] {
			t.Fatalf("missing telemetry event %q; file:\n%s", k, raw)
		}
	}
}

// --- helpers ---

func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
}

// bearer sets the Authorization header on BOTH unary and streaming client calls.
func bearer(token string) connect.Interceptor { return bearerIntc{token: token} }

type bearerIntc struct{ token string }

func (b bearerIntc) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.token)
		return next(ctx, req)
	}
}
func (b bearerIntc) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return conn
	}
}
func (b bearerIntc) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
