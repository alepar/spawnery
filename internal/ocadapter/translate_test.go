package ocadapter

import (
	"encoding/json"
	"testing"
)

func TestParsePartUpdatedAndKind(t *testing.T) {
	raw := json.RawMessage(`{"sessionID":"ses_1","part":{"id":"prt_1","type":"text","text":"hello"},"time":1}`)
	pu, err := ParsePartUpdated(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pu.SessionID != "ses_1" || pu.Part.ID != "prt_1" || pu.Part.Type != "text" {
		t.Fatalf("bad parse: %+v", pu)
	}
	kind, ok := PartTypeToACPKind(pu.Part.Type)
	if !ok || kind != "agent_message_chunk" {
		t.Fatalf("text should map to agent_message_chunk, got %q ok=%v", kind, ok)
	}
}

func TestReasoningMapsToThought(t *testing.T) {
	kind, ok := PartTypeToACPKind("reasoning")
	if !ok || kind != "agent_thought_chunk" {
		t.Fatalf("reasoning -> agent_thought_chunk, got %q ok=%v", kind, ok)
	}
	if _, ok := PartTypeToACPKind("tool"); ok {
		t.Fatal("tool should not map to a text chunk")
	}
}

func TestParsePartDelta(t *testing.T) {
	raw := json.RawMessage(`{"sessionID":"ses_1","messageID":"msg_1","partID":"prt_1","field":"text","delta":"lo"}`)
	d, err := ParsePartDelta(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.PartID != "prt_1" || d.Field != "text" || d.Delta != "lo" {
		t.Fatalf("bad delta parse: %+v", d)
	}
}

func TestACPSessionUpdateShape(t *testing.T) {
	u := ACPSessionUpdate("ses_1", "agent_message_chunk", "hi")
	b, _ := json.Marshal(u)
	s := string(b)
	if !contains(s, `"sessionId":"ses_1"`) || !contains(s, `"sessionUpdate":"agent_message_chunk"`) || !contains(s, `"text":"hi"`) {
		t.Fatalf("bad ACP update json: %s", s)
	}
}

func TestPermissionOptionsAndMapping(t *testing.T) {
	opts := PermissionToACPOptions()
	kinds := map[string]bool{}
	for _, o := range opts {
		kinds[o.Kind] = true
	}
	for _, want := range []string{"allow_once", "allow_always", "reject_once", "reject_always"} {
		if !kinds[want] {
			t.Fatalf("missing ACP option kind %q", want)
		}
	}
	if ACPOptionIDToOpencodeResponse("allow_once") != "once" ||
		ACPOptionIDToOpencodeResponse("allow_always") != "always" ||
		ACPOptionIDToOpencodeResponse("reject_once") != "reject" {
		t.Fatal("optionId -> opencode response mapping wrong")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
