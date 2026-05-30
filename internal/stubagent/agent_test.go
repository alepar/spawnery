package stubagent

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"spawnery/internal/acp"
)

func TestStubPromptEchoes(t *testing.T) {
	in := &bytes.Buffer{}
	acp.WriteMessage(in, acp.Message{ID: ip(1), Method: "initialize"})
	acp.WriteMessage(in, acp.Message{ID: ip(2), Method: "session/new",
		Params: json.RawMessage(`{"cwd":"/data"}`)})
	acp.WriteMessage(in, acp.Message{ID: ip(3), Method: "session/prompt",
		Params: json.RawMessage(`{"text":"hello"}`)})

	out := &bytes.Buffer{}
	if err := Run(io.NopCloser(in), out); err != nil && err != io.EOF {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(out.String(), "ECHO: hello") {
		t.Fatalf("expected echoed update, got: %s", out.String())
	}
}

func ip(i int) *int { return &i }
