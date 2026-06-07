package acp

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestServerReadsRequestAndWritesResponse(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	var out bytes.Buffer
	s := NewServer(in, &out)

	msg, err := s.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	id, ok := msg.ID.AsInt()
	if msg.Method != "initialize" || !ok || id != 1 {
		t.Fatalf("unexpected msg: %+v", msg)
	}
	if err := s.Respond(id, map[string]any{"protocolVersion": 1}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	line, _ := bufio.NewReader(&out).ReadString('\n')
	if !strings.Contains(line, `"id":1`) || !strings.Contains(line, `"protocolVersion":1`) {
		t.Fatalf("bad response line: %s", line)
	}
}

func TestServerWritesNotification(t *testing.T) {
	var out bytes.Buffer
	s := NewServer(strings.NewReader(""), &out)
	if err := s.Notify("session/update", map[string]any{"sessionId": "s1"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"method":"session/update"`) || strings.Contains(got, `"id"`) {
		t.Fatalf("notification should have method and no id: %s", got)
	}
}

func TestServerRespondError(t *testing.T) {
	var out bytes.Buffer
	s := NewServer(strings.NewReader(""), &out)
	if err := s.RespondError(7, -32000, "boom"); err != nil {
		t.Fatalf("respond error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"id":7`) || !strings.Contains(got, `"code":-32000`) || !strings.Contains(got, "boom") {
		t.Fatalf("bad error response: %s", got)
	}
}
