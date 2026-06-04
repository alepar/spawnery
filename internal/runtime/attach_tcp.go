package runtime

import (
	"context"
	"fmt"
	"net"
	"time"
)

// acpDialTimeout bounds how long AttachTCP retries: the agent container (under
// gVisor) may still be booting goose + the adapter when the first client attaches.
const acpDialTimeout = 20 * time.Second

// AttachTCP dials the agent's ACP endpoint over TCP (the pod IP, reachable from
// the host via the CNI bridge) and returns a bidirectional stream whose
// Stdin/Stdout are the same connection. This is the CRI/runsc transport: gVisor
// isolates the in-sandbox abstract-UDS namespace from the host, so the node
// cannot reach the adapter via setns; it dials podIP:port instead. No root needed.
func AttachTCP(ctx context.Context, addr string) (*AttachedStream, error) {
	conn, err := dialTCPRetry(ctx, addr)
	if err != nil {
		return nil, err
	}
	return &AttachedStream{Stdin: conn, Stdout: conn, Close: conn.Close}, nil
}

// dialTCPRetry retries the dial until the adapter is listening or the deadline passes.
func dialTCPRetry(ctx context.Context, addr string) (net.Conn, error) {
	deadline := time.Now().Add(acpDialTimeout)
	var d net.Dialer
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dial ACP tcp %s: %w", addr, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
