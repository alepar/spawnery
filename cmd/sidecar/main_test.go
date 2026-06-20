package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestServeAll_GracefulShutdown verifies that serveAll drains in-flight requests before returning,
// shuts down within the grace window, and closes both listeners.
func TestServeAll_GracefulShutdown(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})

	// Handler that signals it has started, blocks briefly (simulates a long-running inference
	// request), then responds 200.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	srv0 := &http.Server{Addr: "127.0.0.1:0", Handler: handler}
	srv1 := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}

	lns, err := bindAll(srv0, srv1)
	if err != nil {
		t.Fatalf("bindAll: %v", err)
	}

	addr0 := lns[0].Addr().String()
	addr1 := lns[1].Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveAll(ctx, 2*time.Second, []*http.Server{srv0, srv1}, lns)
	}()

	// Fire a request to the first server; it will block inside the handler.
	reqDone := make(chan int, 1)
	go func() {
		// Wait until both servers are accepting (small retry loop for the second server).
		var resp *http.Response
		var reqErr error
		for i := 0; i < 50; i++ {
			resp, reqErr = http.Get(fmt.Sprintf("http://%s/", addr0))
			if reqErr == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if reqErr != nil {
			reqDone <- -1
			return
		}
		resp.Body.Close()
		reqDone <- resp.StatusCode
	}()

	// Wait until the handler has entered (in-flight request is live).
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler to start")
	}

	// Cancel ctx — this triggers shutdown.
	cancel()

	// (1) The in-flight request must complete with 200 (drained, not dropped).
	select {
	case code := <-reqDone:
		if code != http.StatusOK {
			t.Errorf("in-flight request got status %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight request to complete")
	}

	// (2) serveAll must return nil within the grace window.
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("serveAll returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveAll did not return within 3s after shutdown")
	}

	// (3) Both listeners must be closed — fresh dials should be refused.
	for _, addr := range []string{addr0, addr1} {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			t.Errorf("expected dial to %s to be refused after shutdown, but it succeeded", addr)
		}
	}
}
