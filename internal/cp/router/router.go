// Package router is the CP session mux: it relays raw bytes between a client's
// Session stream and the one node stream hosting that spawn, keyed by spawn_id.
// It is a TRANSPARENT relay — it never parses, inspects, or logs frame content.
package router

import (
	"fmt"
	"sync"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
)

type ClientSender interface{ Send([]byte) error }

type route struct {
	nodeID  string
	node    registry.NodeSender
	clients map[string]ClientSender
	done    chan struct{} // closed when the route is dropped (stop or node evict)
}

type Router struct {
	mu sync.Mutex
	m  map[string]*route // spawn_id -> route
}

func New() *Router { return &Router{m: map[string]*route{}} }

// Bind records which node hosts a spawn (after StartSpawn ACTIVE).
func (r *Router) Bind(spawnID, nodeID string, node registry.NodeSender) {
	r.mu.Lock()
	r.m[spawnID] = &route{nodeID: nodeID, node: node, clients: map[string]ClientSender{}, done: make(chan struct{})}
	r.mu.Unlock()
}

// AttachClient registers a client by id and tells the node to open the relay for it (carrying cursor).
func (r *Router) AttachClient(spawnID, clientID string, c ClientSender, cursor int64) (<-chan struct{}, error) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("unknown spawn: %s", spawnID)
	}
	rt.clients[clientID] = c
	node, done := rt.node, rt.done
	r.mu.Unlock()
	return done, node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{SpawnId: spawnID, ClientId: clientID, Cursor: cursor}}})
}

// DetachClient removes a client and tells the node to close its relay (pod stays).
func (r *Router) DetachClient(spawnID, clientID string) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	var wasPresent bool
	if ok {
		_, wasPresent = rt.clients[clientID]
		delete(rt.clients, clientID)
	}
	r.mu.Unlock()
	if wasPresent {
		_ = rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Close{Close: &nodev1.SessionClose{SpawnId: spawnID, ClientId: clientID}}})
	}
}

// FromClient forwards client->agent bytes to the hosting node.
func (r *Router) FromClient(spawnID, clientID string, data []byte) error {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown spawn: %s", spawnID)
	}
	return rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Frame{Frame: &nodev1.Frame{SpawnId: spawnID, ClientId: clientID, Data: data}}})
}

// FromNode forwards an agent->client frame to the addressed client (if still attached).
func (r *Router) FromNode(spawnID, clientID string, data []byte) {
	r.mu.Lock()
	var c ClientSender
	if rt, ok := r.m[spawnID]; ok {
		c = rt.clients[clientID]
	}
	r.mu.Unlock()
	if c != nil {
		_ = c.Send(data)
	}
}

// Drop removes a single spawn's route (on StopSpawn) and unblocks any client.
func (r *Router) Drop(spawnID string) {
	r.mu.Lock()
	if rt, ok := r.m[spawnID]; ok {
		close(rt.done)
		delete(r.m, spawnID)
	}
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
			dropped = append(dropped, id)
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
