package spawnlet_test

import (
	"context"
	"crypto/tls"
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

	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

func TestEndToEndStub(t *testing.T) {
	if os.Getenv("SKIP_DOCKER") != "" {
		t.Skip("SKIP_DOCKER")
	}
	rt, err := runtime.NewDocker()
	if err != nil || rt.Ping(context.Background()) != nil {
		t.Skip("docker unavailable")
	}

	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage:    "spawnery/stubagent:dev",
		SidecarImage:  "spawnery/sidecar:dev",
		OpenRouterKey: "unused",
		DataRoot:      t.TempDir(),
	})
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(spawnlet.NewServer(mgr)))
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.Start()
	defer srv.Close()

	// CRITICAL: srv.Client() is HTTP/1.1 only and cannot handle gRPC bidi streams.
	// Build an h2c client (cleartext HTTP/2) pointing at srv.URL instead.
	hc := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}

	cl := spawnv1connect.NewSpawnServiceClient(hc, srv.URL, connect.WithGRPC())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&spawnv1.CreateSpawnRequest{
		AppPath: mustAbs(t, "../../examples/hello-app"),
		Model:   "x",
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
	if err := c.NewSession("/data"); err != nil {
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

// mustAbs resolves rel relative to this file's directory and returns an absolute path.
func mustAbs(t *testing.T, rel string) string {
	t.Helper()
	// __file__ is internal/spawnlet, so ../../examples/hello-app is correct
	abs, err := filepath.Abs(filepath.Join(".", rel))
	if err != nil {
		t.Fatalf("mustAbs(%q): %v", rel, err)
	}
	return abs
}

// writerTo returns an io.Writer that sends each Write as a Frame on the stream.
type streamWriter struct {
	stream  *connect.BidiStreamForClient[spawnv1.Frame, spawnv1.Frame]
	spawnID string
}

func writerTo(stream *connect.BidiStreamForClient[spawnv1.Frame, spawnv1.Frame], id string) io.Writer {
	return &streamWriter{stream: stream, spawnID: id}
}

func (w *streamWriter) Write(b []byte) (int, error) {
	if err := w.stream.Send(&spawnv1.Frame{SpawnId: w.spawnID, Data: b}); err != nil {
		return 0, err
	}
	return len(b), nil
}
