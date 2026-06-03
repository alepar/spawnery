package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"spawnery/internal/acp"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
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

// readyProbeTimeout bounds how long startSpawn waits for the agent to answer an ACP initialize
// before declaring the spawn failed. Kept well under the CP scheduler's 60s Provision wait
// (cmd/cp/main.go) so the node reports ERROR (with a useful detail) rather than the scheduler
// timing out. goose boots to ACP-ready in ~5s; 30s is generous headroom for a slow node.
const readyProbeTimeout = 30 * time.Second

// probeReady blocks until the spawn's agent answers an ACP initialize, or the timeout elapses. It
// opens its OWN attach (retrying while the attach itself fails — e.g. the CRI in-pod adapter is still
// starting), sends one initialize, and waits for the matching response. The attach is closed before
// returning; detaching does not disturb the long-lived agent (the relay attaches/detaches per client
// session the same way).
func (a *attacher) probeReady(ctx context.Context, sp *spawnlet.Spawn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var att *runtime.AttachedStream
	for {
		var err error
		att, err = a.mgr.Attach(ctx, sp)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("attach agent: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond): // CRI: adapter UDS not listening yet
		}
	}
	defer att.Close()
	return awaitInitialize(ctx, att.Stdin, att.Stdout, time.Until(deadline))
}
