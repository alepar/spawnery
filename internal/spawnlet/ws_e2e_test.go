//go:build e2e

package spawnlet_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// wsIO adapts a websocket.Conn to io.Reader/io.Writer for acp.Client.
// Two wsIO instances can share one conn safely: coder/websocket allows
// concurrent read + write.
type wsIO struct {
	ctx  context.Context
	conn *websocket.Conn
	buf  []byte
}

func (a *wsIO) Read(p []byte) (int, error) {
	for len(a.buf) == 0 {
		_, b, err := a.conn.Read(a.ctx)
		if err != nil {
			return 0, err
		}
		a.buf = b
	}
	n := copy(p, a.buf)
	a.buf = a.buf[n:]
	return n, nil
}

func (a *wsIO) Write(p []byte) (int, error) {
	if err := a.conn.Write(a.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func TestWSEndToEndStub(t *testing.T) {
	rt, err := runtime.NewDocker()
	if err != nil || rt.Ping(context.Background()) != nil {
		t.Fatal("docker required for e2e")
	}
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage: "spawnery/stubagent:dev", SidecarImage: "spawnery/sidecar:dev",
		OpenRouterKey: "unused", DataRoot: t.TempDir(),
	})
	srv := spawnlet.NewServer(mgr)
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(srv))
	mux.HandleFunc("/ws/session", srv.HandleWS)
	hs := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cl := spawnv1connect.NewSpawnServiceClient(hs.Client(), hs.URL)
	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&spawnv1.CreateSpawnRequest{
		AppPath: mustAbs(t, "../../examples/secret-app"), Model: "x",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := cs.Msg.SpawnId
	defer cl.StopSpawn(context.Background(), connect.NewRequest(&spawnv1.StopSpawnRequest{SpawnId: id}))

	conn, _, err := websocket.Dial(ctx, "ws"+hs.URL[len("http"):]+"/ws/session", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 * 1024 * 1024)
	conn.Write(ctx, websocket.MessageText, []byte(`{"spawnId":"`+id+`"}`))

	c := acp.NewClient(&wsIO{ctx: ctx, conn: conn}, &wsIO{ctx: ctx, conn: conn})
	if err := c.Initialize(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := c.NewSession("/app"); err != nil {
		t.Fatalf("session: %v", err)
	}
	var got strings.Builder
	if err := c.Prompt("hello", func(s string) { got.WriteString(s) }); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(got.String(), "ECHO: hello") {
		t.Fatalf("got %q", got.String())
	}
}
