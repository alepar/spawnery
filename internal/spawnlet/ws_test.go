package spawnlet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"spawnery/internal/runtime"
)

func TestWSRelayEchoesViaFake(t *testing.T) {
	f := runtime.NewFake()
	m := NewManager(f, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	srv := NewServer(m)
	// Override the prod default (AttachACP needs root/setns) with the fake's
	// in-process echo stream so this test stays hermetic.
	srv.attach = func(ctx context.Context, sp *Spawn) (*runtime.AttachedStream, error) {
		return f.Attach(ctx, sp.AgentID)
	}
	sp, err := m.Create(context.Background(), "ws-1", writeApp(t), "x") // writeApp from manager_test.go
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/session", srv.HandleWS)
	hs := httptest.NewServer(mux)
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+hs.URL[len("http"):]+"/ws/session", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// bind, then send a frame; the fake runtime's Attach loops stdin->stdout, so it echoes.
	conn.Write(ctx, websocket.MessageText, []byte(`{"spawnId":"`+sp.ID+`"}`))
	conn.Write(ctx, websocket.MessageBinary, []byte("hello\n"))
	_, got, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("want echo, got %q", got)
	}
}
