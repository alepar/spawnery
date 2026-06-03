package node

import (
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []Frame{
		{Seq: 1, Kind: "user", Text: "hello"},
		{Seq: 2, Kind: "agent", Text: "hi"},
		{Seq: 3, Kind: "thought", Text: "hmm"},
		{Seq: 4, Kind: "tool", ToolID: "t1", Title: "read", Status: "in_progress"},
		{Seq: 5, Kind: "turn", State: "busy", Queued: 2},
		{Kind: "perm_request", ReqID: "p1", Title: "allow fs?"},
		{Kind: "reset", FromSeq: 10},
		{Kind: "prompt", Text: "do it"},
		{Kind: "perm_response", ReqID: "p1", Allow: true},
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
		if got != c {
			t.Fatalf("%s: round-trip mismatch: %+v != %+v", c.Kind, got, c)
		}
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := decodeFrame([]byte("not json\n")); err == nil {
		t.Fatal("want error on garbage")
	}
}
