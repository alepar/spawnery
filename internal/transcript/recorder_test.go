package transcript

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeFrame(t *testing.T, frame []byte) []Item {
	t.Helper()
	if len(frame) == 0 {
		t.Fatal("expected a non-empty history frame")
	}
	if frame[len(frame)-1] != '\n' {
		t.Fatalf("frame must be newline-terminated, got %q", string(frame))
	}
	var m struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Items []Item `json:"items"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("frame not valid json: %v\n%s", err, string(frame))
	}
	if m.Jsonrpc != "2.0" || m.Method != "spawn/history" {
		t.Fatalf("frame envelope wrong: jsonrpc=%q method=%q", m.Jsonrpc, m.Method)
	}
	return m.Params.Items
}

func clientPrompt(text string) []byte {
	return []byte(`{"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"` + text + `"}]}}` + "\n")
}
func agentChunk(kind, text string) []byte {
	return []byte(`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"` + kind + `","content":{"type":"text","text":"` + text + `"}}}}` + "\n")
}

func TestRecorderCoalescesTranscript(t *testing.T) {
	r := New()
	r.ObserveClientLine(clientPrompt("hello"))
	r.ObserveAgentLine(agentChunk("agent_message_chunk", "He"))
	r.ObserveAgentLine(agentChunk("agent_message_chunk", "llo!"))
	r.ObserveAgentLine(agentChunk("agent_thought_chunk", "hmm"))
	r.ObserveAgentLine([]byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"search","status":"pending"}}}` + "\n"))
	r.ObserveAgentLine([]byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}}` + "\n"))

	items := decodeFrame(t, r.HistoryFrame())
	want := []Item{
		{Role: "user", Text: "hello"},
		{Role: "agent", Text: "Hello!"},
		{Role: "thought", Text: "hmm"},
		{Role: "tool", Title: "search", Status: "completed"},
	}
	if len(items) != len(want) {
		t.Fatalf("items=%+v want %+v", items, want)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Fatalf("item[%d]=%+v want %+v", i, items[i], want[i])
		}
	}
}

func TestRecorderIgnoresNonAcpAndEmptyIsNilFrame(t *testing.T) {
	r := New()
	if f := r.HistoryFrame(); f != nil {
		t.Fatalf("empty recorder must yield a nil frame, got %q", string(f))
	}
	r.ObserveClientLine([]byte("not json\n"))
	r.ObserveAgentLine([]byte("hello\n"))
	r.ObserveAgentLine([]byte(`{"method":"initialize","id":1}` + "\n"))
	if f := r.HistoryFrame(); f != nil {
		t.Fatalf("recorder must stay empty for non-transcript traffic, got %q", string(f))
	}
}

func TestRecorderCapsAndMarksTruncation(t *testing.T) {
	r := New()
	for i := 0; i < MaxItems+50; i++ {
		r.ObserveClientLine(clientPrompt("p"))
	}
	items := decodeFrame(t, r.HistoryFrame())
	if len(items) != MaxItems {
		t.Fatalf("len=%d want capped at %d", len(items), MaxItems)
	}
	if items[0].Role != "system" || !strings.Contains(items[0].Text, "truncated") {
		t.Fatalf("first item must be the truncation marker, got %+v", items[0])
	}
}
