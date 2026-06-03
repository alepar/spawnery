package main

import (
	"bufio"
	"io"
	"net"
	"sync"
)

// connHub holds the currently-attached client connection (at most one) and serializes all writes to
// it so multi-byte frames (agent output lines AND the replayed history frame) never interleave.
type connHub struct {
	mu      sync.Mutex // guards cur
	cur     net.Conn
	writeMu sync.Mutex // serializes writes to cur; never held while waiting on mu
}

// write sends p to the current client (if any) as one atomic write. Output produced while no client
// is attached is dropped (attach/detach semantics).
func (h *connHub) write(p []byte) {
	h.mu.Lock()
	c := h.cur
	h.mu.Unlock()
	if c == nil {
		return
	}
	h.writeMu.Lock()
	_, _ = c.Write(p) // a dead conn's Write returns fast
	h.writeMu.Unlock()
}

// attach makes c the current connection and, holding writeMu so no pump write can slip in front,
// writes the history frame to it FIRST. Returns the displaced connection (if any) for the caller to
// close. A superseded conn is closed only on a new attach, never on stdin EOF (half-close).
//
// Lock order: attach takes writeMu THEN mu (briefly). write takes mu, releases it, THEN takes
// writeMu — so no goroutine holds mu while waiting on writeMu, and there is no deadlock cycle.
func (h *connHub) attach(c net.Conn, history []byte) net.Conn {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.mu.Lock()
	prev := h.cur
	h.cur = c
	h.mu.Unlock()
	if len(history) > 0 && c != nil {
		_, _ = c.Write(history)
	}
	if prev == c {
		return nil
	}
	return prev
}

// pump is the single persistent reader of the agent's stdout. It reads ndjson lines, records any
// session/update into rec, and forwards each line byte-for-byte to the current client. Non-JSON or
// non-ACP lines are forwarded unchanged and simply not recorded.
func pump(fromAgent io.Reader, hub *connHub, rec *recorder) {
	br := bufio.NewReaderSize(fromAgent, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			rec.observeAgent(line)
			hub.write(line)
		}
		if err != nil {
			return
		}
	}
}

// recordingCopy forwards the client's stdin to the agent line-by-line (byte-for-byte), recording any
// session/prompt into rec. Returns on the client's write-side EOF (full close OR CloseWrite).
// observeClient is called BEFORE writing to the agent so the user item is recorded before any
// agent reply can race into the transcript.
func recordingCopy(toAgent io.Writer, conn io.Reader, rec *recorder) {
	br := bufio.NewReaderSize(conn, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			rec.observeClient(line)
			if _, werr := toAgent.Write(line); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// serve accepts one client at a time and bridges it to the long-lived agent stdio, recording the
// transcript and replaying it (spawn/history) to each newly-attached client. The agent persists
// across client disconnects, so a reconnecting client resumes the same session and gets its history.
// It returns only when the listener is closed.
func serve(ln net.Listener, toAgent io.Writer, fromAgent io.Reader) error {
	hub := &connHub{}
	rec := newRecorder()
	go pump(fromAgent, hub, rec)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		if prev := hub.attach(conn, rec.historyFrame()); prev != nil {
			_ = prev.Close()
		}
		recordingCopy(toAgent, conn, rec)
	}
}
