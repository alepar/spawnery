// Package router is the CP session mux: it relays raw bytes between a client's
// Session stream and the one node stream hosting that spawn, keyed by spawn_id.
// It is a TRANSPARENT relay — it never parses, inspects, or logs frame content.
package router

import (
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
)

type ClientSender interface{ Send([]byte) error }

type route struct {
	nodeID string
	node   registry.NodeSender
	// clients is keyed by (sessionID, clientID): a spawn hosts multiple concurrent sessions whose
	// browser panels may share a clientID (the web uses a module-level CLIENT_ID per panel type), so
	// addressing by clientID alone would let a 2nd session's attach clobber the 1st's sender and
	// misroute agent->client frames (sp-npxq.5). The proto carries session_id on every frame.
	clients  map[clientKey]ClientSender
	sessions []*nodev1.SessionInfo // mirrored roster (node-authoritative)
	done     chan struct{}         // closed when the route is dropped (stop or node evict)
}

// clientKey identifies one attached client within a spawn: the (session, client) pair.
type clientKey struct{ sessionID, clientID string }

// ck builds a clientKey, normalizing an empty sessionID to "0". The ws bind defaults empty -> "0"
// (single-session clients), and the node may emit session-#0 frames with an empty session_id (legacy
// single-session path), so "" and "0" must address the same client.
func ck(sessionID, clientID string) clientKey {
	if sessionID == "" {
		sessionID = "0"
	}
	return clientKey{sessionID, clientID}
}

// pendingRoster is a roster stashed before its spawn's Bind ran. The nodeID is retained so DropNode
// can purge stashes for a node that drops before the spawn ever binds.
type pendingRoster struct {
	nodeID   string
	sessions []*nodev1.SessionInfo
}

type Router struct {
	mu      sync.Mutex
	m       map[string]*route         // spawn_id -> route
	pending map[string]*pendingRoster // rosters that arrived before Bind (node-emits-then-Bind race)
}

func New() *Router {
	return &Router{m: map[string]*route{}, pending: map[string]*pendingRoster{}}
}

// Bind records which node hosts a spawn (after StartSpawn ACTIVE). Any roster the node emitted before
// Bind ran is drained into the new route so ListSessions reflects it immediately.
func (r *Router) Bind(spawnID, nodeID string, node registry.NodeSender) {
	r.mu.Lock()
	rt := &route{nodeID: nodeID, node: node, clients: map[clientKey]ClientSender{}, done: make(chan struct{})}
	if p, ok := r.pending[spawnID]; ok {
		rt.sessions = p.sessions
		delete(r.pending, spawnID)
	}
	r.m[spawnID] = rt
	r.mu.Unlock()
}

// AttachClient registers a client by id and tells the node to open the relay for it (carrying cursor).
func (r *Router) AttachClient(spawnID, sessionID, clientID string, c ClientSender, cursor int64) (<-chan struct{}, error) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("unknown spawn: %s", spawnID)
	}
	rt.clients[ck(sessionID, clientID)] = c
	node, done := rt.node, rt.done
	r.mu.Unlock()
	return done, node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{SpawnId: spawnID, SessionId: sessionID, ClientId: clientID, Cursor: cursor}}})
}

// DetachClient removes a client and tells the node to close its relay (pod stays).
func (r *Router) DetachClient(spawnID, sessionID, clientID string) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	var wasPresent bool
	if ok {
		_, wasPresent = rt.clients[ck(sessionID, clientID)]
		delete(rt.clients, ck(sessionID, clientID))
	}
	r.mu.Unlock()
	if wasPresent {
		_ = rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Close{Close: &nodev1.SessionClose{SpawnId: spawnID, SessionId: sessionID, ClientId: clientID}}})
	}
}

// FromClient forwards client->agent bytes to the hosting node.
func (r *Router) FromClient(spawnID, sessionID, clientID string, data []byte) error {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown spawn: %s", spawnID)
	}
	return rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Frame{Frame: &nodev1.Frame{SpawnId: spawnID, SessionId: sessionID, ClientId: clientID, Data: data}}})
}

// FromNode forwards an agent->client frame to the addressed client (if still attached), keyed by
// (sessionID, clientID) so concurrent sessions sharing a clientID each get their own frames (sp-npxq.5).
func (r *Router) FromNode(spawnID, sessionID, clientID string, data []byte) {
	r.mu.Lock()
	var c ClientSender
	if rt, ok := r.m[spawnID]; ok {
		c = rt.clients[ck(sessionID, clientID)]
	}
	r.mu.Unlock()
	if c != nil {
		_ = c.Send(data)
	}
}

// UpdateRoster replaces the mirrored session set for a spawn. If the route is not bound yet (the node
// emitted the roster before the scheduler's Bind ran), stash it (tagged with nodeID so DropNode can
// purge it) and apply it at Bind.
func (r *Router) UpdateRoster(spawnID, nodeID string, sessions []*nodev1.SessionInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rt, ok := r.m[spawnID]; ok {
		rt.sessions = sessions
		return
	}
	r.pending[spawnID] = &pendingRoster{nodeID: nodeID, sessions: sessions}
}

// ApplySessionStatus updates a single mirrored session's state (membership unchanged).
func (r *Router) ApplySessionStatus(spawnID, sessionID string, state nodev1.SessionState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessionsLocked(spawnID) {
		if s.SessionId == sessionID {
			s.State = state
			return
		}
	}
}

// sessionsLocked returns the live or pending session slice for spawnID (caller holds mu).
func (r *Router) sessionsLocked(spawnID string) []*nodev1.SessionInfo {
	if rt, ok := r.m[spawnID]; ok {
		return rt.sessions
	}
	if p, ok := r.pending[spawnID]; ok {
		return p.sessions
	}
	return nil
}

// ListSessions returns a snapshot of the mirrored roster for the client-facing RPC (nil if unbound).
// Each element is a CLONE: the stored *SessionInfo pointers are mutated in place by ApplySessionStatus
// under the lock, so the RPC must read from copies it owns rather than the shared roster entries.
func (r *Router) ListSessions(spawnID string) []*nodev1.SessionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt, ok := r.m[spawnID]
	if !ok {
		return nil
	}
	out := make([]*nodev1.SessionInfo, len(rt.sessions))
	for i, si := range rt.sessions {
		out[i] = proto.Clone(si).(*nodev1.SessionInfo)
	}
	return out
}

// CreateSession asks the hosting node to launch an additional session.
func (r *Router) CreateSession(spawnID string, transport nodev1.SessionTransport, runnable string) error {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown spawn: %s", spawnID)
	}
	return rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: spawnID, Transport: transport, Runnable: runnable,
	}}})
}

// CloseSession asks the hosting node to reap one session.
func (r *Router) CloseSession(spawnID, sessionID string) error {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown spawn: %s", spawnID)
	}
	return rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_CloseSession{CloseSession: &nodev1.CloseSession{
		SpawnId: spawnID, SessionId: sessionID,
	}}})
}

// Drop removes a single spawn's route (on StopSpawn) and unblocks any client.
func (r *Router) Drop(spawnID string) {
	r.mu.Lock()
	if rt, ok := r.m[spawnID]; ok {
		close(rt.done)
		delete(r.m, spawnID)
	}
	delete(r.pending, spawnID)
	r.mu.Unlock()
}

// DropNode removes every route on a node (on evict) and unblocks their clients.
func (r *Router) DropNode(nodeID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var dropped []string
	for id, rt := range r.m {
		if rt.nodeID == nodeID {
			close(rt.done)
			delete(r.m, id)
			delete(r.pending, id)
			dropped = append(dropped, id)
		}
	}
	// Purge rosters stashed for spawns that never bound before this node dropped (otherwise they leak).
	for id, p := range r.pending {
		if p.nodeID == nodeID {
			delete(r.pending, id)
		}
	}
	return dropped
}

// StopOnNode tells the hosting node to destroy the pod (used by StopSpawn).
func (r *Router) StopOnNode(spawnID string) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	r.mu.Unlock()
	if ok {
		_ = rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Stop{Stop: &nodev1.StopSpawn{SpawnId: spawnID}}})
	}
}
