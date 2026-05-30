//go:build e2e

package spawnlet_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// TestWSEndToEndGooseSecret is the WebSocket analogue of TestEndToEndGooseSecret:
// it drives the REAL Goose agent through the spawnlet's /ws/session WebSocket
// endpoint — the exact transport the browser web client uses — proving the
// browser's flow (CreateSpawn via Connect-JSON, then ACP-over-WS:
// initialize -> session/new -> session/prompt) works against the live agent.
//
// The agent reads the seeded secret out of the scratch mount and recites it;
// a pass proves file mounts + tool-use + live inference + the WS byte relay all
// work together over the web client's wire. Requires Docker and a live
// OPENROUTER_API_KEY; if either is missing it FAILS loudly (no skips).
func TestWSEndToEndGooseSecret(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Fatal("OPENROUTER_API_KEY is required for the WS Goose e2e test")
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
	srv := spawnlet.NewServer(mgr)
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(srv))
	mux.HandleFunc("/ws/session", srv.HandleWS)
	hs := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	// CreateSpawn over Connect-JSON unary, exactly as the web client's fetch does.
	cl := spawnv1connect.NewSpawnServiceClient(hs.Client(), hs.URL)
	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&spawnv1.CreateSpawnRequest{
		AppPath: mustAbs(t, "../../examples/secret-app"),
		Model:   "openai/gpt-oss-120b:free",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := cs.Msg.SpawnId
	defer cl.StopSpawn(context.Background(), connect.NewRequest(&spawnv1.StopSpawnRequest{SpawnId: id}))

	// Dial the WS session and bind the spawn, exactly as the browser does.
	conn, _, err := websocket.Dial(ctx, "ws"+hs.URL[len("http"):]+"/ws/session", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 * 1024 * 1024)
	conn.Write(ctx, websocket.MessageText, []byte(`{"spawnId":"`+id+`"}`))

	// Drive ACP over the WS byte relay: initialize -> session/new -> prompt.
	c := acp.NewClient(&wsIO{ctx: ctx, conn: conn}, &wsIO{ctx: ctx, conn: conn})
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
		t.Fatalf("agent did not recite the secret over WS; got %q", reply)
	}
}
