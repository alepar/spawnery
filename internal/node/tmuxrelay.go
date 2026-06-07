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
	execArgv []string // full argv to attach: <execprefix> <container> tmux attach -t spawn
	send     func(clientID string, data []byte) error

	mu           sync.Mutex
	clients      map[string]*tmuxClient
	lastActivity time.Time // last relay frame in either direction; the idle reaper's clock
}

type tmuxClient struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

func newTmuxRelay(attachArgv []string, send func(clientID string, data []byte) error) *tmuxRelay {
	return &tmuxRelay{execArgv: attachArgv, send: send, clients: map[string]*tmuxClient{}, lastActivity: time.Now()}
}

// markActive refreshes the idle clock. Callers must NOT already hold mu.
func (r *tmuxRelay) markActive() { r.mu.Lock(); r.lastActivity = time.Now(); r.mu.Unlock() }

// lastActive reports the time of the most recent relay frame in either direction.
func (r *tmuxRelay) lastActive() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.lastActivity }

// attached reports whether any client is currently attached.
func (r *tmuxRelay) attached() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.clients) > 0 }

// attach starts a `tmux attach` PTY for clientID and pumps its output back via send.
func (r *tmuxRelay) attach(ctx context.Context, clientID string) error {
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
				r.markActive() // PTY output = activity
				// r.send is synchronous and blocks when the downstream consumer is slow (gRPC stream
				// flow control → WebSocket write → browser). That back-pressure propagates here: a
				// slow browser stalls this read loop, which in turn stalls the PTY read, pausing the
				// agent. No node-side buffering or credit scheme is therefore needed. The only place
				// an unbounded buffer could form is xterm.js's internal write queue in the browser;
				// that is observed by the BacklogTracker wedge metric (sp-9xr.11).
				_ = r.send(clientID, append([]byte(nil), buf[:n]...))
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
	r.mu.Lock()
	c := r.clients[clientID]
	r.mu.Unlock()
	if c == nil {
		return
	}
	r.markActive() // client input = activity
	kind, data, cols, rows := parseClientFrame(b)
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
