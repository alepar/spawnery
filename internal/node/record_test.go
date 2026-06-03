package node

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"spawnery/internal/spawnlet"
	"spawnery/internal/transcript"
)

func TestLineBufferSplitsAcrossChunks(t *testing.T) {
	var lb lineBuffer
	var got []string
	emit := func(b []byte) { got = append(got, string(b)) }
	lb.feed([]byte("ab"), emit)    // no newline yet
	lb.feed([]byte("c\nde"), emit) // emits "abc\n"
	lb.feed([]byte("f\n"), emit)   // emits "def\n"
	if len(got) != 2 || got[0] != "abc\n" || got[1] != "def\n" {
		t.Fatalf("lines=%q want [abc\\n def\\n]", got)
	}
}

func TestRecordingEndpointTeesAndForwardsByteExact(t *testing.T) {
	rec := transcript.New()
	prompt := []byte(`{"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"hi"}]}}` + "\n")
	update := []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ECHO: hi"}}}}` + "\n")

	recvQ := [][]byte{prompt}
	var sent [][]byte
	ep := spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			if len(recvQ) == 0 {
				return nil, io.EOF
			}
			b := recvQ[0]
			recvQ = recvQ[1:]
			return b, nil
		},
		Send: func(b []byte) error { sent = append(sent, append([]byte(nil), b...)); return nil },
	}
	w := recordingEndpoint(ep, rec)

	b, err := w.Recv()
	if err != nil || string(b) != string(prompt) {
		t.Fatalf("recv forward: b=%q err=%v", b, err)
	}
	if err := w.Send(update); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || string(sent[0]) != string(update) {
		t.Fatalf("send forward: %q", sent)
	}

	var m struct {
		Params struct {
			Items []transcript.Item `json:"items"`
		} `json:"params"`
	}
	frame := rec.HistoryFrame()
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("frame: %v\n%s", err, frame)
	}
	if len(m.Params.Items) != 2 ||
		m.Params.Items[0] != (transcript.Item{Role: "user", Text: "hi"}) ||
		m.Params.Items[1] != (transcript.Item{Role: "agent", Text: "ECHO: hi"}) {
		t.Fatalf("transcript=%+v", m.Params.Items)
	}
}

func TestRecorderRegistryGetOrCreateAndRemove(t *testing.T) {
	reg := newRecorderRegistry()
	a := reg.getOrCreate("sp1")
	if a == nil || reg.getOrCreate("sp1") != a {
		t.Fatal("getOrCreate must return the same recorder per spawn id")
	}
	reg.remove("sp1")
	if reg.getOrCreate("sp1") == a {
		t.Fatal("remove must drop the recorder so a fresh one is created")
	}
}

func TestBrokerEndpointQueuesAndDrains(t *testing.T) {
	rec := transcript.New()
	in := make(chan []byte, 8)
	var sentToClient [][]byte
	var sendMu sync.Mutex
	ep := spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			b, ok := <-in
			if !ok {
				return nil, io.EOF
			}
			return b, nil
		},
		Send: func(b []byte) error {
			sendMu.Lock()
			sentToClient = append(sentToClient, append([]byte(nil), b...))
			sendMu.Unlock()
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := brokerEndpoint(ctx, ep, rec)

	in <- []byte(`{"id":1,"method":"session/prompt","params":{"prompt":[{"text":"a"}]}}` + "\n")
	in <- []byte(`{"id":2,"method":"session/prompt","params":{"prompt":[{"text":"b"}]}}` + "\n")

	got1, _ := be.Recv()
	if !bytes.Contains(got1, []byte(`"text":"a"`)) {
		t.Fatalf("first agent-bound line = %q, want prompt a", string(got1))
	}

	if err := be.Send([]byte(`{"id":1,"result":{"stopReason":"end_turn"}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	got2, _ := be.Recv()
	if !bytes.Contains(got2, []byte(`"text":"b"`)) {
		t.Fatalf("second agent-bound line = %q, want drained prompt b", string(got2))
	}

	sendMu.Lock()
	defer sendMu.Unlock()
	var sawTurn bool
	for _, b := range sentToClient {
		if bytes.Contains(b, []byte(`"method":"spawn/turn"`)) {
			sawTurn = true
		}
	}
	if !sawTurn {
		t.Fatalf("expected a spawn/turn frame sent to the client, got %d frames", len(sentToClient))
	}
}
