package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"spawnery/internal/acp"
)

// probeInitID is the JSON-RPC id used by the readiness probe's initialize. It lives only on the
// probe's own throwaway attach (closed before any client connects), so it can't collide with the
// client's request ids.
const probeInitID = 1

func ptrInt(i int) *int { return &i }

// awaitInitialize writes one ACP initialize request to stdin and reads stdout until the matching
// response arrives (the agent is ready), the timeout elapses, or the stream closes. Frames that are
// not our initialize response (e.g. notifications, other ids) are ignored. An error reply to our id
// still counts as "answered" (the agent is up). This is the pure, unit-tested core of probeReady.
func awaitInitialize(ctx context.Context, stdin io.Writer, stdout io.Reader, timeout time.Duration) error {
	req := acp.Message{
		ID:     ptrInt(probeInitID),
		Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`),
	}
	if err := acp.WriteMessage(stdin, req); err != nil { // WriteMessage sets jsonrpc:"2.0" for us
		return fmt.Errorf("write initialize: %w", err)
	}
	done := make(chan error, 1) // buffered so the reader goroutine never leaks after a timeout
	go func() {
		rd := acp.NewReader(stdout)
		for {
			msg, err := rd.ReadMessage()
			if err != nil {
				done <- fmt.Errorf("read initialize response: %w", err)
				return
			}
			if msg.ID != nil && *msg.ID == probeInitID && (msg.Result != nil || msg.Error != nil) {
				done <- nil // the agent answered our initialize -> ready
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("agent did not answer initialize within %s", timeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}
