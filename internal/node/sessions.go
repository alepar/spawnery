package node

import (
	"sort"
	"strconv"
	"sync"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/agentcaps"
)

// SessionZeroID is the well-known id of a spawn's primary (pinned) session — the existing agent.
const SessionZeroID = "0"

// sessionKey routes pumps/relays by (spawn, session). Session #0 uses SessionZeroID.
type sessionKey struct{ spawnID, sessionID string }

type sessionEntry struct {
	id        string
	transport nodev1.SessionTransport
	runnable  string
	state     nodev1.SessionState
	endpoint  string // opaque per-transport handle: acp port (e.g. "7000") or tmux session name
	pinned    bool
}

// sessionRegistry holds the live session set for ONE spawn, keyed by session id. The node owns this
// truth and mirrors it to the CP via SessionRoster. Safe for concurrent use.
type sessionRegistry struct {
	mu       sync.Mutex
	spawnID  string
	sessions map[string]*sessionEntry
	nextID   int // monotonic allocator for non-zero session ids
}

func newSessionRegistry(spawnID string) *sessionRegistry {
	return &sessionRegistry{spawnID: spawnID, sessions: map[string]*sessionEntry{}, nextID: 1}
}

// allocID returns the next free non-zero session id (does not reserve it; register does).
func (r *sessionRegistry) allocID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		id := strconv.Itoa(r.nextID)
		if _, taken := r.sessions[id]; !taken {
			return id
		}
		r.nextID++
	}
}

func (r *sessionRegistry) register(e *sessionEntry) {
	r.mu.Lock()
	r.sessions[e.id] = e
	if n, err := strconv.Atoi(e.id); err == nil && n >= r.nextID {
		r.nextID = n + 1
	}
	r.mu.Unlock()
}

// remove deletes a session; returns false if it was not present.
func (r *sessionRegistry) remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[id]; !ok {
		return false
	}
	delete(r.sessions, id)
	return true
}

func (r *sessionRegistry) get(id string) (*sessionEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sessions[id]
	return e, ok
}

// snapshot returns the session set as proto SessionInfo, session #0 first then ascending by id.
func (r *sessionRegistry) snapshot() []*nodev1.SessionInfo {
	r.mu.Lock()
	out := make([]*nodev1.SessionInfo, 0, len(r.sessions))
	for _, e := range r.sessions {
		out = append(out, &nodev1.SessionInfo{
			SessionId: e.id, Transport: e.transport, Runnable: e.runnable,
			State: e.state, Endpoint: e.endpoint, Pinned: e.pinned,
		})
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].SessionId == SessionZeroID {
			return true
		}
		if out[j].SessionId == SessionZeroID {
			return false
		}
		ni, _ := strconv.Atoi(out[i].SessionId)
		nj, _ := strconv.Atoi(out[j].SessionId)
		return ni < nj
	})
	return out
}

// transportForMode maps a spawn run mode to the session transport for session #0.
// tmux -> mosh PTY relay; acp/served -> ACP Pump (matches startSpawn's only special-case being tmux).
func transportForMode(mode string) nodev1.SessionTransport {
	if mode == string(agentcaps.ModeTmux) {
		return nodev1.SessionTransport_SESSION_TRANSPORT_MOSH
	}
	return nodev1.SessionTransport_SESSION_TRANSPORT_ACP
}
