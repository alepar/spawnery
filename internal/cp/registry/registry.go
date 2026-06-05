// Package registry tracks nodes attached to the CP and their live capacity.
package registry

import (
	"sync"

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
}

type Registry struct {
	mu sync.Mutex
	m  map[string]*Node
}

func New() *Registry { return &Registry{m: map[string]*Node{}} }

func (r *Registry) Add(n *Node)      { r.mu.Lock(); r.m[n.ID] = n; r.mu.Unlock() }
func (r *Registry) Remove(id string) { r.mu.Lock(); delete(r.m, id); r.mu.Unlock() }
func (r *Registry) Get(id string) (*Node, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.m[id]
	return n, ok
}

func (r *Registry) Heartbeat(id string, active, free uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.m[id]; ok {
		n.Free = free
	}
}

// Placement carries the spawn's owner. Node eligibility is the TENANCY rule (sp-cf0): a cloud node is
// multi-tenant (accepts any owner); a self-hosted node is single-tenant (only its bound owner's spawns).
type Placement struct {
	Owner string
}

// eligibleForOwner reports whether a node may run a spawn owned by owner, per the tenancy rule.
func eligibleForOwner(n *Node, owner string) bool {
	if n.Class == "self-hosted" {
		return n.Owner == owner // single-tenant: only its own owner
	}
	return true // cloud (and the unset default) is multi-tenant
}

// PickFor returns the eligible node with the most free capacity for the placement, or nil.
func (r *Registry) PickFor(p Placement) *Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *Node
	for _, n := range r.m {
		if n.Free == 0 {
			continue
		}
		if !eligibleForOwner(n, p.Owner) {
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
