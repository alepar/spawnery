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

// acpPoolLo/acpPoolHi bound the per-spawn additional-session ACP port pool: the highest 100-port
// block BELOW the 32768 Linux ephemeral boundary, so in-container listeners can't collide with
// kernel-assigned ephemeral source ports (spec §4). Session #0 keeps port 7000 (outside the pool).
const (
	acpPoolLo = 32668
	acpPoolHi = 32767
)

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
	nextID   int            // monotonic allocator for non-zero session ids
	ports    map[int]string // acp pool port -> owning session id (the only session allowed to free it)

	// onFreePort, when non-nil, is invoked after every freePort call (after the lock is released). It is
	// a TEST-ONLY observation seam (nil in production, so production behavior is unchanged) that lets a
	// close-during-launch race test deterministically wait for the in-flight launch goroutine's undo
	// freePort to complete before asserting on the port map.
	onFreePort func()
}

func newSessionRegistry(spawnID string) *sessionRegistry {
	return &sessionRegistry{spawnID: spawnID, sessions: map[string]*sessionEntry{}, nextID: 1, ports: map[int]string{}}
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

// allocPort reserves the lowest-free port in [acpPoolLo, acpPoolHi] for owner (the acp session id);
// ok=false when the pool is exhausted (caller rejects CreateSession). Reservation is atomic under the
// registry lock. The owner is recorded so only that session may later free the port (see freePort).
func (r *sessionRegistry) allocPort(owner string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p := acpPoolLo; p <= acpPoolHi; p++ {
		if _, taken := r.ports[p]; !taken {
			r.ports[p] = owner
			return p, true
		}
	}
	return 0, false
}

// freePort releases acp pool port p, but ONLY if it is still owned by owner — making the free
// ownership-safe and idempotent. This prevents a double-free across a realloc: when a CloseSession and
// the in-flight launch goroutine's mid-launch undo both try to free the same port, the first free wins
// and the second is a no-op; and if the port was reallocated to a DIFFERENT session in between, a stale
// owner's free cannot steal the new owner's reservation.
func (r *sessionRegistry) freePort(p int, owner string) {
	r.mu.Lock()
	if r.ports[p] == owner {
		delete(r.ports, p)
	}
	r.mu.Unlock()
	if r.onFreePort != nil {
		r.onFreePort()
	}
}

// portOwner returns the session id that currently owns acp pool port p ("" if free). A locked accessor
// so tests can assert on pool ownership without racing the registry's internal ports map.
func (r *sessionRegistry) portOwner(p int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ports[p]
}

// setState transitions a session's state under the lock (so snapshot never races a field write);
// returns false if the id is gone (e.g. closed mid-launch).
func (r *sessionRegistry) setState(id string, st nodev1.SessionState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sessions[id]
	if !ok {
		return false
	}
	e.state = st
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
