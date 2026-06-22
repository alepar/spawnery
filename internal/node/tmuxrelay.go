package node

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

// Client→node tmux frame opcodes (first byte). node→client frames are raw terminal bytes.
const (
	tmuxOpInput  byte = 0x00 // rest = raw stdin bytes
	tmuxOpResize byte = 0x01 // rest = ASCII "<cols> <rows>"
)

// tmuxAttachRetryTimeout / tmuxAttachRetryInterval bound the relay's defense-in-depth has-session
// poll inside attach(): if the session is momentarily absent (residual race after the Part 1 gate, or
// a session restart), we retry briefly rather than forwarding "no sessions" to the client PTY and
// dying. After the timeout we fall through to the real attach — whatever tmux says becomes the
// client's problem, but we never hang forever (sp-m859.4).
const (
	tmuxAttachRetryTimeout  = 3 * time.Second
	tmuxAttachRetryInterval = 150 * time.Millisecond
)

// parseClientFrame splits a client→node tmux frame into (opcode, inputData, cols, rows). An empty
// frame is treated as a no-op input.
func parseClientFrame(b []byte) (kind byte, data []byte, cols, rows uint16) {
	if len(b) == 0 {
		return tmuxOpInput, nil, 0, 0
	}
	switch b[0] {
	case tmuxOpResize:
		fields := strings.Fields(string(b[1:]))
		if len(fields) == 2 {
			c, _ := strconv.Atoi(fields[0])
			r, _ := strconv.Atoi(fields[1])
			return tmuxOpResize, nil, uint16(c), uint16(r)
		}
		return tmuxOpResize, nil, 0, 0
	default:
		return tmuxOpInput, b[1:], 0, 0
	}
}

// tmuxRelay brokers terminal clients for one tmux-mode spawn. There is no shared backing process
// (tmux in the container is the shared state); each attached client gets its own `tmux attach` PTY.
type tmuxRelay struct {
	execArgv   []string // full argv to attach: <execprefix> <container> tmux attach -t spawn
	send       func(clientID string, data []byte) error
	hasSession func(ctx context.Context) (bool, error) // optional; if set, attach() polls before pty.Start

	mu            sync.Mutex
	clients       map[string]*tmuxClient
	lastActivity  time.Time // last relay frame in either direction; the idle reaper's clock
	forkBarrier   *forkIngressBarrier
	queuedInput   map[string][][]byte
	activeOutput  int
	outputDrained chan struct{}
}

type tmuxClient struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

func newTmuxRelay(attachArgv []string, send func(clientID string, data []byte) error) *tmuxRelay {
	outputDrained := make(chan struct{})
	close(outputDrained)
	return &tmuxRelay{
		execArgv:      attachArgv,
		send:          send,
		clients:       map[string]*tmuxClient{},
		queuedInput:   map[string][][]byte{},
		lastActivity:  time.Now(),
		outputDrained: outputDrained,
	}
}

// withHasSession attaches a has-session check to the relay (sp-m859.4): attach() polls this function
// before calling pty.Start, retrying briefly if the session is absent. Returns r for chaining.
func (r *tmuxRelay) withHasSession(fn func(ctx context.Context) (bool, error)) *tmuxRelay {
	r.hasSession = fn
	return r
}

// markActive refreshes the idle clock. Callers must NOT already hold mu.
func (r *tmuxRelay) markActive() { r.mu.Lock(); r.lastActivity = time.Now(); r.mu.Unlock() }

// lastActive reports the time of the most recent relay frame in either direction.
func (r *tmuxRelay) lastActive() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.lastActivity }

// attached reports whether any client is currently attached.
func (r *tmuxRelay) attached() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.clients) > 0 }

func (r *tmuxRelay) tryAcquireForkBarrier(b forkIngressBarrier) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forkBarrier != nil {
		return r.forkBarrier.matches(b)
	}
	bb := b
	r.forkBarrier = &bb
	return true
}

func (r *tmuxRelay) beginOutput() {
	r.mu.Lock()
	if r.activeOutput == 0 {
		r.outputDrained = make(chan struct{})
	}
	r.activeOutput++
	r.mu.Unlock()
}

func (r *tmuxRelay) endOutput() {
	r.mu.Lock()
	if r.activeOutput > 0 {
		r.activeOutput--
		if r.activeOutput == 0 && r.outputDrained != nil {
			close(r.outputDrained)
		}
	}
	r.mu.Unlock()
}

func (r *tmuxRelay) waitOutputDrained(ctx context.Context) error {
	r.mu.Lock()
	drained := r.outputDrained
	r.mu.Unlock()
	if drained == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-drained:
		return nil
	}
}

func (r *tmuxRelay) releaseForkBarrier(match func(forkIngressBarrier) bool) {
	type pendingWrite struct {
		ptmx *os.File
		data []byte
	}
	var writes []pendingWrite
	r.mu.Lock()
	if r.forkBarrier != nil && match(*r.forkBarrier) {
		r.forkBarrier = nil
		for clientID, chunks := range r.queuedInput {
			c := r.clients[clientID]
			if c == nil {
				continue
			}
			for _, chunk := range chunks {
				writes = append(writes, pendingWrite{ptmx: c.ptmx, data: chunk})
			}
		}
		clear(r.queuedInput)
	}
	r.mu.Unlock()
	for _, w := range writes {
		_, _ = w.ptmx.Write(w.data)
	}
}

// attach starts a `tmux attach` PTY for clientID and pumps its output back via send.
// Defense-in-depth (sp-m859.4): if hasSession is set, poll it briefly before pty.Start so the relay
// never forwards "no sessions" to the client PTY on a transient race. The poll is bounded by
// tmuxAttachRetryTimeout; if the session still isn't present after that we fall through and let tmux
// itself respond — we never hang indefinitely.
func (r *tmuxRelay) attach(ctx context.Context, clientID string) error {
	if r.hasSession != nil {
		deadline := time.Now().Add(tmuxAttachRetryTimeout)
		for {
			ok, _ := r.hasSession(ctx)
			if ok {
				break // session present: proceed to pty.Start
			}
			if ctx.Err() != nil || time.Now().After(deadline) {
				break // bounded: fall through to the real attach regardless
			}
			select {
			case <-ctx.Done():
			case <-time.After(tmuxAttachRetryInterval):
			}
			// After waking: loop to re-check hasSession; ctx.Err() above catches cancellation.
		}
	}
	cmd := exec.CommandContext(ctx, r.execArgv[0], r.execArgv[1:]...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.clients[clientID] = &tmuxClient{ptmx: ptmx, cmd: cmd}
	r.mu.Unlock()
	r.markActive() // attach = activity
	go func() {
		// Reap the `docker exec` child once the read loop ends (EOF or external detach/stop closed
		// the PTY), so killed execs don't accumulate as zombies over many attach/detach cycles.
		defer func() { _ = cmd.Wait() }()
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				r.beginOutput()
				r.markActive() // PTY output = activity
				// r.send is synchronous and blocks when the downstream consumer is slow (gRPC stream
				// flow control → WebSocket write → browser). That back-pressure propagates here: a
				// slow browser stalls this read loop, which in turn stalls the PTY read, pausing the
				// agent. No node-side buffering or credit scheme is therefore needed. The only place
				// an unbounded buffer could form is xterm.js's internal write queue in the browser;
				// that is observed by the BacklogTracker wedge metric (sp-9xr.11).
				_ = r.send(clientID, append([]byte(nil), buf[:n]...))
				r.endOutput()
			}
			if err != nil {
				r.detach(clientID)
				return
			}
		}
	}()
	return nil
}

func (r *tmuxRelay) fromClient(clientID string, b []byte) {
	kind, data, cols, rows := parseClientFrame(b)
	r.mu.Lock()
	c := r.clients[clientID]
	blocked := c != nil && kind == tmuxOpInput && r.forkBarrier != nil
	if c != nil {
		r.lastActivity = time.Now()
	}
	if blocked {
		r.queuedInput[clientID] = append(r.queuedInput[clientID], append([]byte(nil), data...))
	}
	r.mu.Unlock()
	if c == nil || blocked {
		return
	}
	switch kind {
	case tmuxOpResize:
		if cols > 0 && rows > 0 {
			_ = pty.Setsize(c.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
		}
	default:
		_, _ = c.ptmx.Write(data)
	}
}

func (r *tmuxRelay) detach(clientID string) {
	r.mu.Lock()
	c := r.clients[clientID]
	delete(r.clients, clientID)
	r.mu.Unlock()
	if c != nil {
		_ = c.ptmx.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	}
}

func (r *tmuxRelay) stop() {
	r.mu.Lock()
	ids := make([]string, 0, len(r.clients))
	for id := range r.clients {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.detach(id)
	}
}
