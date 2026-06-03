package main

import (
	"bufio"
	"io"
	"net"
	"sync"

	"spawnery/internal/transcript"
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

// pump is the single persistent reader of the agent's stdout. It records each ndjson line, forwards
// it byte-for-byte to the current client, and — when a line is the in-flight prompt's turn-end
// response — drains the next queued prompt to the agent (via agentCh) and pushes a spawn/turn frame.
func pump(fromAgent io.Reader, hub *connHub, rec *transcript.Recorder, agentCh chan<- []byte) {
	br := bufio.NewReaderSize(fromAgent, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			drain, turn := rec.OnAgentLine(line)
			for _, d := range drain {
				agentCh <- d
			}
			if turn != nil {
				hub.write(turn)
			}
			hub.write(line)
		}
		if err != nil {
			return
		}
	}
}

// recordingCopy reads the client's stdin line-by-line and asks the broker what to forward: idle
// prompts and non-prompt lines go to the agent (via agentCh); prompts received while busy are held
// and queued. spawn/turn frames are written back to the client. Returns on the client's write EOF.
func recordingCopy(conn io.Reader, rec *transcript.Recorder, agentCh chan<- []byte, hub *connHub) {
	br := bufio.NewReaderSize(conn, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			fwd, turn := rec.OnClientLine(line)
			for _, f := range fwd {
				agentCh <- f
			}
			if turn != nil {
				hub.write(turn)
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
	rec := transcript.New()
	agentCh := make(chan []byte, 64)
	// Single writer to agent stdin (forwarded client prompts + drained queued prompts). If
	// toAgent.Write fails the agent stdin is dead: the goroutine retires and producers block on a
	// full agentCh — acceptable because the adapter process exits when the agent subprocess exits.
	go func() {
		for line := range agentCh {
			if _, err := toAgent.Write(line); err != nil {
				return
			}
		}
	}()
	go pump(fromAgent, hub, rec, agentCh)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		if prev := hub.attach(conn, rec.HistoryFrame()); prev != nil {
			_ = prev.Close()
		}
		recordingCopy(conn, rec, agentCh, hub)
	}
}
