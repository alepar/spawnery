package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeAgent returns (toAgent, fromAgent) wired as an echo: bytes written to
// toAgent come back on fromAgent. Models a persistent agent process stdio.
func fakeAgent() (io.Writer, io.Reader) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { _, _ = io.Copy(outW, inR) }() // echo stdin -> stdout, lives for the whole test
	return inW, outR
}

func dialAndRoundtrip(t *testing.T, path, line string) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, line); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != line {
		t.Fatalf("got %q want %q", got, line)
	}
}

func TestServeBridgesAndPersistsAcrossReconnect(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	toAgent, fromAgent := fakeAgent()
	go serve(ln, toAgent, fromAgent)

	// First client.
	dialAndRoundtrip(t, sock, "hello\n")
	// Reconnect: a NEW client must reach the SAME persistent agent.
	dialAndRoundtrip(t, sock, "world\n")
}

func TestServeClientHalfCloseStopsStdinNotStdout(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	toAgent, fromAgent := fakeAgent()
	go serve(ln, toAgent, fromAgent)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}
	// Half-close the write side; the echo of "ping\n" must still arrive.
	if err := c.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read after half-close: %v", err)
	}
	if got != "ping\n" {
		t.Fatalf("got %q want %q", got, "ping\n")
	}
}

// A reconnecting client must receive a spawn/history frame replaying the transcript that flowed
// through the adapter while a PRIOR client was attached. Uses a scripted agent that emits a valid
// ACP session/update for each input line, so traffic in both directions is recordable.
func TestServeReplaysHistoryOnReconnect(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	toAgent, fromAgent := scriptedAgent()
	go serve(ln, toAgent, fromAgent)

	// First client: send a session/prompt; the scripted agent replies with an agent chunk.
	c1, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	prompt := `{"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"hi"}]}}` + "\n"
	if _, err := io.WriteString(c1, prompt); err != nil {
		t.Fatal(err)
	}
	// drain the agent's reply on c1 so the recorder has observed it before we reconnect.
	_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := bufio.NewReader(c1).ReadString('\n'); err != nil {
		t.Fatalf("c1 read agent reply: %v", err)
	}
	_ = c1.Close()

	// Reconnect: the FIRST bytes the new client receives must be the spawn/history frame.
	c2, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(c2).ReadString('\n')
	if err != nil {
		t.Fatalf("c2 read history: %v", err)
	}
	var m struct {
		Method string `json:"method"`
		Params struct {
			Items []item `json:"items"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("history frame not json: %v\n%s", err, line)
	}
	if m.Method != "spawn/history" {
		t.Fatalf("first frame method=%q want spawn/history", m.Method)
	}
	if len(m.Params.Items) < 2 || m.Params.Items[0].Role != "user" || m.Params.Items[0].Text != "hi" {
		t.Fatalf("history items wrong: %+v", m.Params.Items)
	}
	hasAgent := false
	for _, it := range m.Params.Items {
		if it.Role == "agent" {
			hasAgent = true
		}
	}
	if !hasAgent {
		t.Fatalf("history must include the agent reply, got %+v", m.Params.Items)
	}
}

// scriptedAgent echoes each newline-delimited input line back as an agent_message_chunk session/update
// line, so traffic in both directions is valid ACP for the recorder. Lives for the whole test.
func scriptedAgent() (io.Writer, io.Reader) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		br := bufio.NewReader(inR)
		for {
			_, err := br.ReadString('\n')
			if err != nil {
				_ = outW.Close()
				return
			}
			_, _ = io.WriteString(outW, `{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ack"}}}}`+"\n")
		}
	}()
	return inW, outR
}
