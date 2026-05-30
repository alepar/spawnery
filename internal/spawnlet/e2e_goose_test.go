//go:build e2e

package spawnlet_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// TestEndToEndGooseSecret exercises the whole Goose harness end to end: the
// agent reads /app/AGENTS.md in place (cwd=/app), uses its file-read tool
// against the scratch mount at /app/data to find an unguessable secret word
// seeded into README.md, and recites it back. A pass proves file
// mounts + tool-use + instructions + live inference + the byte relay all work
// together. It requires Docker and a live OPENROUTER_API_KEY; if either is
// missing it FAILS loudly (no skips) so a broken env is detected.
func TestEndToEndGooseSecret(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Fatal("OPENROUTER_API_KEY is required for the Goose e2e test")
	}

	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}

	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage:    "spawnery/goose:dev",
		SidecarImage:  "spawnery/sidecar:dev",
		OpenRouterKey: key,
		DataRoot:      t.TempDir(),
	})
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(spawnlet.NewServer(mgr)))
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.Start()
	defer srv.Close()

	// srv.Client() is HTTP/1.1 only; build an h2c (cleartext HTTP/2) client so
	// the gRPC bidi Session stream works.
	hc := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}

	cl := spawnv1connect.NewSpawnServiceClient(hc, srv.URL, connect.WithGRPC())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&spawnv1.CreateSpawnRequest{
		AppPath: mustAbs(t, "../../examples/secret-app"),
		Model:   "openai/gpt-oss-120b:free",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := cs.Msg.SpawnId
	defer cl.StopSpawn(context.Background(), connect.NewRequest(&spawnv1.StopSpawnRequest{SpawnId: id}))

	stream := cl.Session(ctx)
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

	c := acp.NewClient(pr, writerTo(stream, id))
	if err := c.Initialize(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := c.NewSession("/app"); err != nil {
		t.Fatalf("session: %v", err)
	}
	var got strings.Builder
	if err := c.Prompt("What is the secret word?", func(s string) { got.WriteString(s) }); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	reply := got.String()
	t.Logf("agent reply: %q", reply)
	if !strings.Contains(reply, "QUOKKA-4417") {
		t.Fatalf("agent did not recite the secret; got %q", reply)
	}
}
