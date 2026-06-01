//go:build e2e

package cp_test

import (
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
	"spawnery/internal/acp"
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
	authn := auth.New(map[string]string{"dev-token": "alice"})
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
	c := acp.NewClient(pr, frameWriter{stream: stream, id: id})
	if err := c.Initialize(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := c.NewSession("/app"); err != nil {
		t.Fatalf("session: %v", err)
	}
	var got strings.Builder
	if err := c.Prompt("say hi", func(s string) { got.WriteString(s) }); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(got.String(), "ECHO: say hi") {
		t.Fatalf("want ECHO, got %q", got.String())
	}

	stream.CloseRequest()
	time.Sleep(500 * time.Millisecond) // let session_end + StopSpawn flush
	assertTelemetry(t, telPath, id)
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

// frameWriter adapts the cp.v1 Session stream to io.Writer for acp.Client.
type frameWriter struct {
	stream *connect.BidiStreamForClient[cpv1.Frame, cpv1.Frame]
	id     string
}

func (w frameWriter) Write(b []byte) (int, error) {
	if err := w.stream.Send(&cpv1.Frame{SpawnId: w.id, Data: b}); err != nil {
		return 0, err
	}
	return len(b), nil
}
