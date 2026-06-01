package main

import (
	"io"
	"net"
	"sync"
)

// connHub holds the currently-attached client connection (at most one). The
// stdout pump writes agent output to whichever client is current; output
// produced while no client is attached is dropped (attach/detach semantics).
type connHub struct {
	mu  sync.Mutex
	cur net.Conn
}

// set makes c the current connection, returning the connection it displaced
// (if any) so the caller can close it. A superseded connection is closed only
// when a new client attaches, never on stdin EOF — this preserves half-close
// semantics: a client may CloseWrite its stdin while still reading agent output.
func (h *connHub) set(c net.Conn) net.Conn {
	h.mu.Lock()
	prev := h.cur
	h.cur = c
	h.mu.Unlock()
	if prev == c {
		return nil
	}
	return prev
}

func (h *connHub) write(p []byte) {
	h.mu.Lock()
	c := h.cur
	h.mu.Unlock()
	if c != nil {
		_, _ = c.Write(p) // a dead conn's Write returns fast; no lock held here
	}
}

// pump is the single persistent reader of the agent's stdout.
func pump(fromAgent io.Reader, hub *connHub) {
	buf := make([]byte, 32*1024)
	for {
		n, err := fromAgent.Read(buf)
		if n > 0 {
			hub.write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// serve accepts one client at a time and bridges it to the long-lived agent
// stdio. The agent persists across client disconnects, so a reconnecting client
// resumes the same session. It returns only when the listener is closed.
func serve(ln net.Listener, toAgent io.Writer, fromAgent io.Reader) error {
	hub := &connHub{}
	// pump is intentionally not joined: it runs until fromAgent hits EOF/error
	// (the agent exits), independent of serve returning. A future caller that
	// embeds serve in a larger process must not assume serve returning means
	// this goroutine is gone.
	go pump(fromAgent, hub)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		// Attach the new client, displacing (and closing) any prior one. The
		// stdin copy below returns on the client's write-side EOF (full close
		// OR CloseWrite), but we do NOT close conn here: it stays attached in
		// the hub so agent output can keep flowing to a half-closed client.
		// The connection is finally closed when the next client supersedes it
		// (above) or when the listener closes.
		if prev := hub.set(conn); prev != nil {
			_ = prev.Close()
		}
		_, _ = io.Copy(toAgent, conn) // client -> agent stdin; returns on client EOF/close
	}
}
