package spawnlet

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func TestRelayCopiesBothWays(t *testing.T) {
	// client<->spawnlet modeled as channels; agent as in-memory pipes.
	toAgent := make(chan []byte, 8)
	fromAgent := make(chan []byte, 8)

	agentIn, agentInW := io.Pipe()
	agentOut := &bytes.Buffer{}
	agentOut.WriteString("hi-from-agent")

	ep := StreamEndpoint{
		Recv: func() ([]byte, error) {
			b, ok := <-toAgent
			if !ok {
				return nil, io.EOF
			}
			return b, nil
		},
		Send: func(b []byte) error { fromAgent <- append([]byte{}, b...); return nil },
	}
	stdio := AgentIO{Stdin: agentInW, Stdout: io.MultiReader(bytes.NewReader([]byte("hi-from-agent")))}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go Relay(ctx, ep, stdio)

	toAgent <- []byte("hi-from-client")
	close(toAgent)

	got := <-fromAgent
	if string(got) != "hi-from-agent" {
		t.Fatalf("agent->client got %q", got)
	}
	// drain what client sent into the agent pipe
	buf := make([]byte, 64)
	n, _ := agentIn.Read(buf)
	if string(buf[:n]) != "hi-from-client" {
		t.Fatalf("client->agent got %q", buf[:n])
	}
}
