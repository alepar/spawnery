package node

import (
	"io"
	"sync"
)

const defaultMaxLog = 2000 // cap the per-spawn frame log; oldest trimmed (a lagging client gets reset)

type client struct {
	cursor int64 // last seq this client has been sent
	send   frameSender
	notify chan struct{} // buffered(1): "catch up"
	done   chan struct{}
}

// frameSender delivers one encoded frame line to a client; returns an error if the client is gone.
type frameSender func(line []byte) error

// Pump is the long-lived per-spawn relay: it owns the goose stdio, an append-only frame log, and a
// set of client subscribers. Built across Tasks 2-4. All mutable fields behind mu.
type Pump struct {
	stdin  io.Writer
	stdout io.Reader

	mu      sync.Mutex
	log     []Frame // log[i].Seq == base+1+i (contiguous)
	base    int64   // seq of the last trimmed frame (0 = nothing trimmed)
	seq     int64   // last assigned seq
	maxLog  int
	clients map[string]*client
	stopped bool // set by stop() in Task 4 (agent teardown); unused in the fan-out core
}

func newPump(stdin io.Writer, stdout io.Reader) *Pump {
	return &Pump{stdin: stdin, stdout: stdout, maxLog: defaultMaxLog, clients: map[string]*client{}}
}

// appendFrames assigns seqs, appends to the log (trimming the oldest past maxLog), and wakes clients.
func (p *Pump) appendFrames(fs []Frame) {
	if len(fs) == 0 {
		return
	}
	p.mu.Lock()
	for i := range fs {
		p.seq++
		fs[i].Seq = p.seq
		p.log = append(p.log, fs[i])
	}
	if over := len(p.log) - p.maxLog; over > 0 {
		p.base += int64(over)
		p.log = append(p.log[:0], p.log[over:]...)
	}
	for _, c := range p.clients {
		wake(c)
	}
	p.mu.Unlock()
}

func wake(c *client) {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (p *Pump) attachClient(clientID string, cursor int64, send frameSender) {
	p.mu.Lock()
	if old := p.clients[clientID]; old != nil {
		close(old.done) // replace same id (defensive; normally ids are unique per connection)
	}
	c := &client{cursor: cursor, send: send, notify: make(chan struct{}, 1), done: make(chan struct{})}
	p.clients[clientID] = c
	p.mu.Unlock()
	wake(c) // initial catch-up
	go p.clientLoop(c)
}

func (p *Pump) detachClient(clientID string) {
	p.mu.Lock()
	if c := p.clients[clientID]; c != nil {
		close(c.done)
		delete(p.clients, clientID)
	}
	p.mu.Unlock()
}

// clientLoop ships log frames > c.cursor whenever woken, until done.
func (p *Pump) clientLoop(c *client) {
	for {
		select {
		case <-c.done:
			return
		case <-c.notify:
		}
		for {
			p.mu.Lock()
			// If the client's cursor is outside the retained window [base, seq] -- it missed trimmed
			// frames (cursor < base) OR it is ahead of us (cursor > seq, e.g. the pump restarted and
			// seq reset to 0 while the client kept an old cursor) -- reset it to base and replay.
			var reset *Frame
			if c.cursor < p.base || c.cursor > p.seq {
				r := Frame{Kind: "reset", FromSeq: p.base}
				reset = &r
				c.cursor = p.base
			}
			var batch []Frame
			if c.cursor < p.seq {
				from := c.cursor - p.base // index of first unseen frame
				batch = append(batch, p.log[from:]...)
				c.cursor = p.seq
			}
			p.mu.Unlock()
			if reset == nil && len(batch) == 0 {
				break
			}
			if reset != nil {
				if c.send(encodeFrame(*reset)) != nil {
					return
				}
			}
			for _, f := range batch {
				if c.send(encodeFrame(f)) != nil {
					return
				}
			}
		}
	}
}
