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
	nodeID string
	owner  string
	node   registry.NodeSender
	client ClientSender
	done   chan struct{} // closed when the route is dropped (stop or node evict)
}

type Router struct {
	mu sync.Mutex
	m  map[string]*route // spawn_id -> route
}

func New() *Router { return &Router{m: map[string]*route{}} }

// Bind records which node hosts a spawn and its owner (after StartSpawn ACTIVE).
func (r *Router) Bind(spawnID, nodeID, owner string, node registry.NodeSender) {
	r.mu.Lock()
	r.m[spawnID] = &route{nodeID: nodeID, owner: owner, node: node, done: make(chan struct{})}
	r.mu.Unlock()
}

func (r *Router) Owner(spawnID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt, ok := r.m[spawnID]
	if !ok {
		return "", false
	}
	return rt.owner, true
}

// AttachClient binds a live client stream and tells the node to open the relay.
// The returned channel closes if the route is dropped while attached.
func (r *Router) AttachClient(spawnID string, c ClientSender) (<-chan struct{}, error) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("unknown spawn: %s", spawnID)
	}
	rt.client = c
	node := rt.node
	done := rt.done
	r.mu.Unlock()
	return done, node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{SpawnId: spawnID}}})
}

// DetachClient clears the client and tells the node to detach (pod stays).
func (r *Router) DetachClient(spawnID string) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	if ok {
		rt.client = nil
	}
	r.mu.Unlock()
	if ok {
		_ = rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Close{Close: &nodev1.SessionClose{SpawnId: spawnID}}})
	}
}

// FromClient forwards client->agent bytes to the hosting node.
func (r *Router) FromClient(spawnID string, data []byte) error {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown spawn: %s", spawnID)
	}
	return rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Frame{Frame: &nodev1.Frame{SpawnId: spawnID, Data: data}}})
}

// FromNode forwards agent->client bytes to the attached client (if any).
func (r *Router) FromNode(spawnID string, data []byte) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	var c ClientSender
	if ok {
		c = rt.client
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
