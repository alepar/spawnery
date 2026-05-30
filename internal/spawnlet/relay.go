package spawnlet

import (
	"context"
	"io"
)

// StreamEndpoint abstracts the client side (e.g. a Connect bidi stream).
type StreamEndpoint struct {
	Recv func() ([]byte, error) // from client
	Send func([]byte) error     // to client
}

// AgentIO is the agent container's stdio.
type AgentIO struct {
	Stdin  io.Writer
	Stdout io.Reader
}

// Relay copies bytes both ways until either side ends.
func Relay(ctx context.Context, ep StreamEndpoint, io_ AgentIO) {
	done := make(chan struct{}, 2)
	// client -> agent
	go func() {
		for {
			b, err := ep.Recv()
			if err != nil {
				done <- struct{}{}
				return
			}
			if len(b) > 0 {
				if _, werr := io_.Stdin.Write(b); werr != nil {
					done <- struct{}{}
					return
				}
			}
		}
	}()
	// agent -> client
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := io_.Stdout.Read(buf)
			if n > 0 {
				if serr := ep.Send(buf[:n]); serr != nil {
					done <- struct{}{}
					return
				}
			}
			if err != nil {
				done <- struct{}{}
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}
