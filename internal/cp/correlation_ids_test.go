package cp

// TestCorrelationIDs verifies that spawn_id and session_id are present in log output
// from handlers that attach them to the context — the mechanism under test is the
// slogctx.With* family wired into the request path, not the helpers themselves.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

// syncBuf is a goroutine-safe bytes.Buffer for capturing log output written from server goroutines.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hasLogField returns true if any JSON log line in output contains key==value.
func hasLogField(output, key, value string) bool {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m[key] == value {
			return true
		}
	}
	return false
}

// TestSetSpawnModel_SpawnIDInLog verifies that spawn_id appears in logs emitted by SetSpawnModel.
// This exercises the context enrichment added at line "ctx = slogctx.WithSpawnID(ctx, spawnID)":
// any slogctx.FromContext(ctx) call within the handler will carry the spawn_id field.
func TestSetSpawnModel_SpawnIDInLog(t *testing.T) {
	var buf syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	s, reg, _ := newTestServer(t)
	sender := &ackSender{models: s.models, ok: true}
	activeSpawnOnNode(t, s, reg, "sp-log-spawn", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.SetSpawnModel(ctx, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp-log-spawn", Model: "new-model"}))
	if err != nil {
		t.Fatalf("SetSpawnModel: %v", err)
	}

	if !hasLogField(buf.String(), "spawn_id", "sp-log-spawn") {
		t.Fatalf("expected spawn_id=sp-log-spawn in log output; got:\n%s", buf.String())
	}
}

// TestHandleWS_SessionIDInLog verifies that session_id (and spawn_id) appear in log output
// emitted from within the WS session path. The trigger is a normal bind frame against a spawn
// that has no live router route (no node in this hermetic test). The WS handler attempts
// s.rt.AttachClient which returns "unknown spawn", emitting an error log via
// slogctx.FromContext(sessCtx) — that context must carry both IDs after the fix in ws.go.
func TestHandleWS_SessionIDInLog(t *testing.T) {
	var buf syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	priv := genWSKey(t)
	s, _, _, ts := newWSTestServer(t, priv, false, nil)

	ctx := context.Background()
	spawnID := seedSpawn(t, ctx, s, "acct-corrlog")

	// Use time.Now() so the token is not expired at verification time.
	tokenWire := wsMintToken(t, priv, "acct-corrlog", "tok-corrlog", "cp", time.Now())

	wsURL := "ws" + ts.URL[len("http"):]
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// Send a bind frame with an explicit sessionId. The spawn exists in the store but is not
	// bound to any router route (no live node in this hermetic test), so AttachClient will fail
	// and emit an error log via slogctx.FromContext(sessCtx). After the ws.go fix, sessCtx
	// carries both spawn_id and session_id, so both must appear in the log output.
	bind := map[string]any{
		"spawnId":  spawnID,
		"token":    tokenWire,
		"clientId": "c-corrlog",
		"sessionId": "sess-corrlog-42",
	}
	if err := wsjson.Write(ctx, conn, bind); err != nil {
		t.Fatalf("send bind: %v", err)
	}

	// Give the server goroutine time to process the bind frame and emit the log.
	time.Sleep(100 * time.Millisecond)

	logs := buf.String()
	if !hasLogField(logs, "session_id", "sess-corrlog-42") {
		t.Fatalf("expected session_id=sess-corrlog-42 in log output; got:\n%s", logs)
	}
	if !hasLogField(logs, "spawn_id", spawnID) {
		t.Fatalf("expected spawn_id=%s in log output; got:\n%s", spawnID, logs)
	}
}
