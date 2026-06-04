package main

import (
	"bufio"
	"io"
	"net"
	"sync"
)

// maxBufBytes bounds the in-pod gap buffer. Goose stdout produced while no node is attached is held
// here and flushed to the next connection. Past the cap the OLDEST WHOLE LINES are evicted, so a
// wedged or absent node can never OOM the pod and a partial/torn JSON line is never delivered.
const maxBufBytes = 1 << 20 // 1 MiB

// connHub holds the at-most-one attached node connection plus a bounded buffer of goose stdout lines
// produced while nothing is attached. A single mutex serializes the live-vs-buffer decision in
// write() against the swap+flush in attach(), so byte order is preserved with no interleaving.
type connHub struct {
	mu  sync.Mutex
	cur net.Conn
	buf [][]byte // whole ndjson lines held while cur == nil, FIFO
	n   int      // total bytes currently in buf
}

// write sends one ndjson line to the attached node connection, or buffers it if none is attached.
func (h *connHub) write(line []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur != nil {
		_, _ = h.cur.Write(line) // a dead conn's Write returns fast; detach() swaps cur to nil
		return
	}
	b := append([]byte(nil), line...)
	h.buf = append(h.buf, b)
	h.n += len(b)
	for h.n > maxBufBytes && len(h.buf) > 1 { // evict oldest whole lines; never drop the only line
		h.n -= len(h.buf[0])
		h.buf = h.buf[1:]
	}
}

// attach makes c the current connection and flushes the gap buffer to it FIRST, in order, then
// clears it. Returns the displaced connection (if any) for the caller to close.
//
// LOCK TRADEOFF: the flush runs while holding h.mu. That is deliberate. Holding the lock across
// "set cur; flush buf; clear buf" guarantees strict ordering — any concurrent write() is forced
// either before the flush (it appended to buf, so it gets flushed) or after it (cur != nil, so it
// goes live, strictly behind the flushed bytes). No live line slips in front of the buffer and
// nothing interleaves. The cost is head-of-line blocking: while the flush runs, write() cannot take
// the lock, so the stdout pump stops draining goose and a slow reattaching node briefly stalls the
// agent's stdout. We accept that because the flush is bounded (<= maxBufBytes) and goes to a local
// abstract UDS only on reconnect — tiny and rare. The off-lock alternative (snapshot under lock,
// write outside it, queue concurrent writes behind a flushing flag) removes the stall at a
// complexity cost not worth it here.
func (h *connHub) attach(c net.Conn) net.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.cur
	h.cur = c
	for _, line := range h.buf {
		_, _ = c.Write(line)
	}
	h.buf = nil
	h.n = 0
	if prev == c {
		return nil
	}
	return prev
}

// detach clears the current connection if it is still c (so subsequent stdout buffers) and closes c.
func (h *connHub) detach(c net.Conn) {
	h.mu.Lock()
	if h.cur == c {
		h.cur = nil
	}
	h.mu.Unlock()
	_ = c.Close()
}

// pump is the single persistent reader of the agent's stdout. It forwards each ndjson line to the
// attached node connection (or the gap buffer). It does NOT parse or record — the node pump owns all
// brokering, history, and fan-out.
func pump(fromAgent io.Reader, hub *connHub) {
	br := bufio.NewReaderSize(fromAgent, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			hub.write(line)
		}
		if err != nil {
			return
		}
	}
}

// serve accepts one node connection at a time and bridges it to the long-lived agent stdio: goose
// stdout flows to the connection (pump), the connection flows to goose stdin (io.Copy). The agent
// persists across connection drops; output produced during a gap is buffered and flushed on the next
// attach. It returns only when the listener is closed.
func serve(ln net.Listener, toAgent io.Writer, fromAgent io.Reader) error {
	hub := &connHub{}
	go pump(fromAgent, hub)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		if prev := hub.attach(conn); prev != nil {
			_ = prev.Close()
		}
		_, _ = io.Copy(toAgent, conn) // node -> goose stdin; returns when the node closes its conn
		hub.detach(conn)
	}
}
