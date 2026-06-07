package node

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cost := 0.0123
	cases := []Frame{
		// existing kinds (must stay identical)
		{Seq: 1, Kind: "user", Text: "hello"},
		{Seq: 2, Kind: "agent", Text: "hi"},
		{Seq: 3, Kind: "thought", Text: "hmm"},
		{Seq: 4, Kind: "tool", ToolID: "t1", Title: "read", Status: "in_progress"},
		{Seq: 5, Kind: "turn", State: "busy", Queued: 2},
		{Kind: "perm_request", ReqID: "p1", Title: "allow fs?"},
		{Kind: "reset", FromSeq: 10},
		{Kind: "prompt", Text: "do it"},
		{Kind: "perm_response", ReqID: "p1", Allow: true},
		// new typed payloads
		{Seq: 6, Kind: "tool", ToolID: "t2", Tool: &ToolPayload{
			Content:   []ContentBlock{{Type: "text", Text: "out"}},
			Diff:      &Diff{Path: "a.go", OldText: "x", NewText: "y"},
			RawInput:  json.RawMessage(`{"a":1}`),
			RawOutput: json.RawMessage(`{"b":2}`),
		}},
		{Seq: 7, Kind: "plan", Plan: []PlanEntry{{Content: "step", Priority: "high", Status: "pending"}}},
		{Seq: 8, Kind: "turn", State: "idle", Reason: "end_turn", Usage: &Usage{Input: 10, Output: 20, Total: 30, Cost: &cost}},
		{Seq: 9, Kind: "turn", State: "idle", Error: &ErrorInfo{Code: 1, Message: "boom"}},
		{Seq: 10, Kind: "commands", Cmds: []Command{{Name: "/test", Description: "run tests", InputHint: "[pkg]"}}},
		{Seq: 11, Kind: "mode", Mode: &ModePayload{Current: "code", Available: []Mode{{ID: "code", Name: "Code"}, {ID: "ask", Name: "Ask"}}}},
		{Kind: "perm_request", ReqID: "p2", Title: "edit?", Options: []PermOption{{OptionID: "allow_once", Name: "Allow once", Kind: "allow_once"}}},
		{Kind: "perm_response", ReqID: "p2", OptionID: "allow_once"},
		{Kind: "cancel"},
		{Kind: "set_mode", ModeID: "ask"},
	}
	for _, c := range cases {
		line := encodeFrame(c)
		if !strings.HasSuffix(string(line), "\n") {
			t.Fatalf("%s: not newline-terminated", c.Kind)
		}
		got, err := decodeFrame(line)
		if err != nil {
			t.Fatalf("%s: decode: %v", c.Kind, err)
		}
		if !reflect.DeepEqual(got, c) {
			t.Fatalf("%s: round-trip mismatch:\n got  %+v\n want %+v", c.Kind, got, c)
		}
	}
}

// TestFrameByteStable_ExistingKinds pins the wire bytes of the pre-existing kinds. Adding the new
// optional payload fields MUST NOT change them — the seq-log / replay / reset machinery depends on it.
func TestFrameByteStable_ExistingKinds(t *testing.T) {
	cases := []struct {
		f    Frame
		want string
	}{
		{Frame{Seq: 1, Kind: "user", Text: "hi"}, `{"seq":1,"kind":"user","text":"hi"}`},
		{Frame{Seq: 2, Kind: "agent", Text: "yo"}, `{"seq":2,"kind":"agent","text":"yo"}`},
		{Frame{Seq: 3, Kind: "thought", Text: "hmm"}, `{"seq":3,"kind":"thought","text":"hmm"}`},
		{Frame{Seq: 4, Kind: "tool", ToolID: "t1", Title: "read", Status: "in_progress"}, `{"seq":4,"kind":"tool","toolId":"t1","title":"read","status":"in_progress"}`},
		{Frame{Seq: 5, Kind: "turn", State: "busy", Queued: 2}, `{"seq":5,"kind":"turn","state":"busy","queued":2}`},
		{Frame{Kind: "perm_request", ReqID: "p1", Title: "allow?"}, `{"kind":"perm_request","title":"allow?","reqId":"p1"}`},
		{Frame{Kind: "reset", FromSeq: 10}, `{"kind":"reset","fromSeq":10}`},
		{Frame{Kind: "prompt", Text: "go"}, `{"kind":"prompt","text":"go"}`},
		{Frame{Kind: "perm_response", ReqID: "p1", Allow: true}, `{"kind":"perm_response","reqId":"p1","allow":true}`},
	}
	for _, tc := range cases {
		got := strings.TrimRight(string(encodeFrame(tc.f)), "\n")
		if got != tc.want {
			t.Fatalf("%s: wire bytes changed:\n got  %s\n want %s", tc.f.Kind, got, tc.want)
		}
	}
}

// TestEncodeFrameMarshalErrorEmitsNothing covers the marshal-error path: a Frame whose
// Tool.RawInput is invalid JSON makes json.Marshal fail. encodeFrame must NOT emit a corrupt
// frame or a bare newline — it surfaces the error (via the package logger) and returns nil bytes.
func TestEncodeFrameMarshalErrorEmitsNothing(t *testing.T) {
	f := Frame{Seq: 1, Kind: "tool", ToolID: "t1", Tool: &ToolPayload{
		RawInput: json.RawMessage("{invalid"),
	}}
	// Sanity: this Frame really does fail to marshal.
	if _, err := json.Marshal(f); err == nil {
		t.Fatal("precondition: expected json.Marshal to fail on invalid RawInput")
	}
	got := encodeFrame(f)
	if got != nil {
		t.Fatalf("want nil (no frame emitted) on marshal error, got %q", string(got))
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := decodeFrame([]byte("not json\n")); err == nil {
		t.Fatal("want error on garbage")
	}
}
