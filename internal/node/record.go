package node

import (
	"bytes"
	"context"
	"io"
	"sync"

	"spawnery/internal/spawnlet"
	"spawnery/internal/transcript"
)

// recorderRegistry holds one long-lived transcript.Recorder per spawn. It outlives client
// reconnects (a browser reload reuses the recorder) and CP reconnects (it is created in node.Run,
// not in the per-connection attacher). Entries are removed only when the spawn is stopped.
//
// TODO: on a node crash where Stop is never received, the entry leaks (bounded by MaxSpawns). A
// post-demo cleanup could prune entries whose spawnID is no longer in mgr.Store() on CP reconnect.
type recorderRegistry struct {
	mu  sync.Mutex
	rec map[string]*transcript.Recorder
}

func newRecorderRegistry() *recorderRegistry {
	return &recorderRegistry{rec: map[string]*transcript.Recorder{}}
}

func (r *recorderRegistry) getOrCreate(id string) *transcript.Recorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rc := r.rec[id]; rc != nil {
		return rc
	}
	rc := transcript.New()
	r.rec[id] = rc
	return rc
}

func (r *recorderRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.rec, id)
	r.mu.Unlock()
}

// lineBuffer accumulates byte chunks and emits each complete newline-terminated ndjson line. The
// relay forwards opaque chunks; the recorder needs whole lines.
type lineBuffer struct{ buf []byte }

func (l *lineBuffer) feed(p []byte, emit func([]byte)) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return
		}
		line := append([]byte(nil), l.buf[:i+1]...)
		emit(line)
		// Reslice retains the backing array until the next append-realloc. For ACP ndjson (short
		// messages bounded by the relay's read-buffer size) this is fine.
		l.buf = l.buf[i+1:]
	}
}

// brokerEndpoint wraps a StreamEndpoint with the transcript broker. The client->agent direction is
// gated: an internal reader pulls client bytes, splits ndjson lines, and asks the broker what to
// forward (idle prompts pass; prompts while busy are held + queued). All agent-bound bytes — both
// forwarded client prompts and drained queued prompts — flow through agentCh, so Recv (the relay's
// single client->agent goroutine) remains the sole writer to agent stdin. spawn/turn frames are sent
// to the client via ep.Send. Every channel op selects on ctx.Done() (the relay's context) so no
// goroutine leaks if the session tears down while the agent isn't draining its stdin. agentCh is
// never closed.
func brokerEndpoint(ctx context.Context, ep spawnlet.StreamEndpoint, rec *transcript.Recorder) spawnlet.StreamEndpoint {
	agentCh := make(chan []byte, 64)
	var clientLB, agentLB lineBuffer
	go func() {
		for {
			b, err := ep.Recv()
			if len(b) > 0 {
				clientLB.feed(b, func(line []byte) {
					fwd, turn := rec.OnClientLine(line)
					for _, f := range fwd {
						select {
						case agentCh <- f:
						case <-ctx.Done():
							return
						}
					}
					if turn != nil {
						_ = ep.Send(turn)
					}
				})
			}
			if err != nil {
				return
			}
		}
	}()
	return spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			select {
			case b := <-agentCh:
				return b, nil
			case <-ctx.Done():
				return nil, io.EOF
			}
		},
		Send: func(b []byte) error {
			if len(b) > 0 {
				agentLB.feed(b, func(line []byte) {
					drain, turn := rec.OnAgentLine(line)
					for _, d := range drain {
						select {
						case agentCh <- d:
						case <-ctx.Done():
							return
						}
					}
					if turn != nil {
						_ = ep.Send(turn)
					}
				})
			}
			return ep.Send(b)
		},
	}
}

// recordingEndpoint wraps a StreamEndpoint to TEE its bytes into rec without altering the forwarded
// stream: Recv (client->agent) -> ObserveClientLine; Send (agent->client) -> ObserveAgentLine. Each
// direction has its own lineBuffer touched by a single goroutine (Relay runs Recv and Send in
// separate goroutines), and the recorder is internally mutex-guarded.
func recordingEndpoint(ep spawnlet.StreamEndpoint, rec *transcript.Recorder) spawnlet.StreamEndpoint {
	var clientLB, agentLB lineBuffer
	return spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			b, err := ep.Recv()
			if len(b) > 0 {
				clientLB.feed(b, rec.ObserveClientLine)
			}
			return b, err
		},
		Send: func(b []byte) error {
			if len(b) > 0 {
				agentLB.feed(b, rec.ObserveAgentLine)
			}
			return ep.Send(b)
		},
	}
}
