package node

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

type closeLogOrderingStream struct {
	output           *bytes.Buffer
	loggedBeforeSend bool
}

func (s *closeLogOrderingStream) Send(*nodev1.NodeMessage) error {
	s.loggedBeforeSend = strings.Contains(s.output.String(), `"msg":"session_authorization_closed"`)
	return nil
}

func (*closeLogOrderingStream) Receive() (*nodev1.CPMessage, error) { return nil, nil }

func TestAttachmentCloseStructuredLog(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	stream := &closeLogOrderingStream{output: &output}
	a := newAttacher(nil, stream)
	key := sessionAuthKey{spawnID: "sp-sensitive", sessionID: "session-sensitive", clientID: "client-sensitive"}
	const reason = "node authorization revoked"
	a.closeClientAuthorization(key, 17, reason, "attachment-sensitive")
	if !stream.loggedBeforeSend {
		t.Fatal("SessionAuthClosed was sent before its structured close record")
	}

	var matching []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode structured log: %v", err)
		}
		if record["msg"] == "session_authorization_closed" {
			matching = append(matching, record)
		}
	}
	if len(matching) != 1 {
		t.Fatalf("session_authorization_closed records = %d, want 1; output=%s", len(matching), output.String())
	}
	want := map[string]any{
		"spawn_id":      "sp-sensitive",
		"generation":    float64(17),
		"session_id":    "session-sensitive",
		"client_id":     "client-sensitive",
		"attachment_id": "attachment-sensitive",
		"reason":        reason,
	}
	for field, value := range want {
		if got := matching[0][field]; got != value {
			t.Errorf("%s = %#v, want %#v", field, got, value)
		}
	}

	serialized := strings.ToLower(output.String())
	for _, forbidden := range []string{
		"access_token", "node_access_token", "signed_intent", "intent_bytes",
		"private_key", "session_key", "bearer ", "-----begin private key-----",
	} {
		if strings.Contains(serialized, forbidden) {
			t.Errorf("structured close log contains forbidden credential marker %q", forbidden)
		}
	}
}
