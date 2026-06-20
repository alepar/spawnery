package cp

import (
	"context"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
)

// TestServerShutdownDrainsAttachStream verifies that Server.Shutdown pre-empts a blocked
// Attach (runNode) loop and drains cleanly within the deadline, returning nil.
func TestServerShutdownDrainsAttachStream(t *testing.T) {
	s, reg, _ := newTestServer(t)

	// Feed one Register message so runNode reaches the blocked recv().
	registerCh := make(chan *nodev1.NodeMessage, 1)
	registerCh <- &nodev1.NodeMessage{
		Msg: &nodev1.NodeMessage_Register{
			Register: &nodev1.Register{NodeId: "n-shutdown-test", MaxSpawns: 1, NodeClass: "self-hosted"},
		},
	}

	// After the Register is consumed, subsequent recv() calls block indefinitely.
	idleBlock := make(chan struct{})
	recv := func() (*nodev1.NodeMessage, error) {
		// Drain the register message first.
		select {
		case m, ok := <-registerCh:
			if !ok {
				return nil, context.Canceled
			}
			return m, nil
		default:
		}
		// Then block until idleBlock is closed (by Shutdown → shutdownCh → pump → here).
		<-idleBlock
		return nil, context.Canceled
	}

	// done is closed when runNode exits.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.runNode(context.Background(), &capSender{}, recv)
	}()

	// Wait for the node to register before signalling shutdown.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := reg.Get("n-shutdown-test"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never registered before shutdown")
		}
		time.Sleep(time.Millisecond)
	}

	// Signal the blocking recv to unblock when shutdown triggers (Shutdown closes shutdownCh
	// which makes the pump goroutine return via shutdownCh select arm; after the pump exits
	// idleBlock can remain blocked for the pump goroutine, but runNode itself returns via the
	// shutdownCh select arm in the main loop).
	// We close idleBlock so the pump goroutine also unblocks after runNode returns.
	go func() {
		// Unblock the pump goroutine once runNode has exited via shutdownCh.
		<-done
		close(idleBlock)
	}()

	sdCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.Shutdown(sdCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	select {
	case <-done:
		// runNode exited cleanly — good.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runNode goroutine did not exit after Shutdown")
	}
}

// TestServerShutdownIdempotent verifies that calling Shutdown twice does not panic
// (double-close of shutdownCh is guarded by sync.Once) and both calls return nil.
func TestServerShutdownIdempotent(t *testing.T) {
	s, _, _ := newTestServer(t)

	ctx := context.Background()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}
