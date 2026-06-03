package transcript

import (
	"bytes"
	"encoding/json"
	"strconv"
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

func decodeHistory(t *testing.T, frame []byte) ([]Item, string, int) {
	t.Helper()
	var m struct {
		Params struct {
			Items []Item `json:"items"`
			Turn  struct {
				State  string `json:"state"`
				Queued int    `json:"queued"`
			} `json:"turn"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("history not json: %v\n%s", err, string(frame))
	}
	return m.Params.Items, m.Params.Turn.State, m.Params.Turn.Queued
}

// promptID builds a session/prompt request line carrying a JSON-RPC id.
func promptID(id int, text string) []byte {
	return []byte(`{"id":` + strconv.Itoa(id) + `,"method":"session/prompt","params":{"prompt":[{"type":"text","text":"` + text + `"}]}}` + "\n")
}
func response(id int, stopReason string) []byte {
	return []byte(`{"id":` + strconv.Itoa(id) + `,"result":{"stopReason":"` + stopReason + `"}}` + "\n")
}

func decodeTurnFrame(t *testing.T, frame []byte) (string, int) {
	t.Helper()
	if len(frame) == 0 || frame[len(frame)-1] != '\n' {
		t.Fatalf("expected newline-terminated spawn/turn frame, got %q", string(frame))
	}
	var m struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			State  string `json:"state"`
			Queued int    `json:"queued"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("turn frame not json: %v\n%s", err, string(frame))
	}
	if m.Jsonrpc != "2.0" || m.Method != "spawn/turn" {
		t.Fatalf("turn envelope wrong: jsonrpc=%q method=%q", m.Jsonrpc, m.Method)
	}
	return m.Params.State, m.Params.Queued
}

func TestBrokerForwardsAndTracksTurn(t *testing.T) {
	r := New()
	fwd, turn := r.OnClientLine(promptID(1, "hello"))
	if len(fwd) != 1 || !bytes.Equal(fwd[0], promptID(1, "hello")) {
		t.Fatalf("idle prompt must be forwarded once, got %d lines", len(fwd))
	}
	if st, q := decodeTurnFrame(t, turn); st != "busy" || q != 0 {
		t.Fatalf("turn after idle prompt = %s/%d, want busy/0", st, q)
	}
	drain, turn := r.OnAgentLine(response(1, "end_turn"))
	if len(drain) != 0 {
		t.Fatalf("no queued prompts, drain must be empty, got %d", len(drain))
	}
	if st, q := decodeTurnFrame(t, turn); st != "idle" || q != 0 {
		t.Fatalf("turn after response = %s/%d, want idle/0", st, q)
	}
}

func TestBrokerQueuesWhileBusyThenDrains(t *testing.T) {
	r := New()
	r.OnClientLine(promptID(1, "first"))
	fwd, turn := r.OnClientLine(promptID(2, "second"))
	if len(fwd) != 0 {
		t.Fatalf("prompt while busy must be held, got %d forwarded", len(fwd))
	}
	if st, q := decodeTurnFrame(t, turn); st != "busy" || q != 1 {
		t.Fatalf("turn after queued prompt = %s/%d, want busy/1", st, q)
	}
	items, state, queued := decodeHistory(t, r.HistoryFrame())
	if state != "busy" || queued != 1 {
		t.Fatalf("history turn = %s/%d, want busy/1", state, queued)
	}
	if len(items) != 2 || items[1].Role != "user" || !items[1].Pending {
		t.Fatalf("second user item must be pending, items=%+v", items)
	}
	drain, turn := r.OnAgentLine(response(1, "end_turn"))
	if len(drain) != 1 || !bytes.Equal(drain[0], promptID(2, "second")) {
		t.Fatalf("must drain the queued prompt, got %d lines", len(drain))
	}
	if st, q := decodeTurnFrame(t, turn); st != "busy" || q != 0 {
		t.Fatalf("turn after drain = %s/%d, want busy/0", st, q)
	}
	items, _, _ = decodeHistory(t, r.HistoryFrame())
	if items[1].Pending {
		t.Fatalf("drained item must no longer be pending, items=%+v", items)
	}
}

func TestBrokerNonPromptLinesPassThrough(t *testing.T) {
	r := New()
	r.OnClientLine(promptID(1, "go"))
	perm := []byte(`{"id":7,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}` + "\n")
	fwd, turn := r.OnClientLine(perm)
	if len(fwd) != 1 || !bytes.Equal(fwd[0], perm) {
		t.Fatalf("non-prompt client line must pass through unchanged")
	}
	if turn != nil {
		t.Fatalf("non-prompt line must not emit a turn frame")
	}
}

func TestBrokerCancelledResponseEndsTurn(t *testing.T) {
	r := New()
	r.OnClientLine(promptID(3, "x"))
	_, turn := r.OnAgentLine(response(3, "cancelled"))
	if st, _ := decodeTurnFrame(t, turn); st != "idle" {
		t.Fatalf("cancelled stopReason must end the turn, got %s", st)
	}
}

func TestBrokerNonResponseAgentLineKeepsTurn(t *testing.T) {
	r := New()
	r.OnClientLine(promptID(1, "x"))
	drain, turn := r.OnAgentLine([]byte(`{"id":1,"method":"initialize","result":{}}` + "\n"))
	if len(drain) != 0 || turn != nil {
		t.Fatalf("a method-bearing agent line must not end the turn (drain=%d, turn=%v)", len(drain), turn != nil)
	}
	if _, state, _ := decodeHistory(t, r.HistoryFrame()); state != "busy" {
		t.Fatalf("turn must remain busy, got %s", state)
	}
}
