// Package registry tracks nodes attached to the CP and their live capacity.
package registry

import (
	"sync"
	"time"

	nodev1 "spawnery/gen/node/v1"
)

// NodeSender is the CP->node side of a node's Attach stream (concurrency-safe).
type NodeSender interface{ Send(*nodev1.CPMessage) error }

type Node struct {
	ID     string
	Sender NodeSender
	Max    uint32
	Free   uint32
	Images []string
	Class  string
	Owner  string

	token    uint64    // per-connection identity, so a displaced stream's teardown is a no-op
	lastBeat time.Time // last Register/Heartbeat; drives the alive/dead decision for duplicate ids
}

// defaultLiveWindow: a node is "alive" if it registered or heartbeated within this window. The node
// heartbeats every 5s, so 15s tolerates two missed beats before a returning node may displace it.
const defaultLiveWindow = 15 * time.Second

type Registry struct {
	mu         sync.Mutex
	m          map[string]*Node
	nextTok    uint64
	liveWindow time.Duration
	now        func() time.Time // injectable for tests
}

func New() *Registry {
	return &Registry{m: map[string]*Node{}, liveWindow: defaultLiveWindow, now: time.Now}
}

// Add unconditionally registers n (overwriting any existing id), marking it alive now. Used by tests
// and internal seeding; the live node path uses Register, which enforces the displace-only-if-dead
// policy.
func (r *Registry) Add(n *Node) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextTok++
	n.token = r.nextTok
	n.lastBeat = r.now()
	r.m[n.ID] = n
}

// Register adds n under its id and returns a per-connection token plus whether it was accepted.
// Policy for a duplicate id: if the existing node is still ALIVE (heartbeated within liveWindow) the
// new registration is REJECTED (accepted=false), because two streams sharing an id corrupt routing.
// If the existing entry is stale/dead, the new connection silently DISPLACES it.
func (r *Registry) Register(n *Node) (token uint64, accepted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.m[n.ID]; ok && r.now().Sub(cur.lastBeat) < r.liveWindow {
		return 0, false // existing node is alive -> reject the duplicate
	}
	r.nextTok++
	n.token = r.nextTok
	n.lastBeat = r.now()
	r.m[n.ID] = n
	return n.token, true
}

func (r *Registry) Remove(id string) { r.mu.Lock(); delete(r.m, id); r.mu.Unlock() }

// RemoveIfCurrent deletes id only if it is still held by token (this connection), returning whether
// it did. A displaced/rejected connection's teardown is therefore a no-op and never drops the live
// node's routes.
func (r *Registry) RemoveIfCurrent(id string, token uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.m[id]; ok && n.token == token {
		delete(r.m, id)
		return true
	}
	return false
}

// IsCurrent reports whether id is held by token (this connection is the live owner).
func (r *Registry) IsCurrent(id string, token uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.m[id]
	return ok && n.token == token
}

func (r *Registry) Get(id string) (*Node, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.m[id]
	return n, ok
}

// Heartbeat refreshes free capacity and liveness for id, but only from the current owner (token),
// so a displaced/zombie stream's heartbeats are ignored.
func (r *Registry) Heartbeat(id string, token uint64, active, free uint32) {
	_ = active
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.m[id]; ok && n.token == token {
		n.Free = free
		n.lastBeat = r.now()
	}
}

// Placement constrains node selection. An empty field is unconstrained.
type Placement struct {
	Class string
	Owner string
}

// PickFor returns the node with the most free capacity that satisfies the placement, or nil.
func (r *Registry) PickFor(p Placement) *Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *Node
	for _, n := range r.m {
		if n.Free == 0 {
			continue
		}
		if p.Class != "" && n.Class != p.Class {
			continue
		}
		if p.Owner != "" && n.Owner != p.Owner {
			continue
		}
		if best == nil || n.Free > best.Free {
			best = n
		}
	}
	return best
}

// Pick returns the node with the most free slots (>0), or nil if none.
func (r *Registry) Pick() *Node { return r.PickFor(Placement{}) }
