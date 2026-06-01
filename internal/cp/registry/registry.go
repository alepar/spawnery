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

// Pick returns the node with the most free slots (>0), or nil if none.
func (r *Registry) Pick() *Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *Node
	for _, n := range r.m {
		if n.Free == 0 {
			continue
		}
		if best == nil || n.Free > best.Free {
			best = n
		}
	}
	return best
}
