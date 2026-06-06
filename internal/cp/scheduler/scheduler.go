// Package scheduler assigns a spawn to a node, issues StartSpawn over that
// node's stream, and blocks until the node reports ACTIVE (or ERROR/timeout).
package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
)

type Scheduler struct {
	reg     *registry.Registry
	rt      *router.Router
	timeout time.Duration

	mu      sync.Mutex
	pending map[string]chan nodev1.SpawnPhase // spawn_id -> ACTIVE/ERROR signal
}

func New(reg *registry.Registry, rt *router.Router, timeout time.Duration) *Scheduler {
	return &Scheduler{reg: reg, rt: rt, timeout: timeout, pending: map[string]chan nodev1.SpawnPhase{}}
}

// OnStatus is called by the node receive loop when a SpawnStatus arrives.
func (s *Scheduler) OnStatus(spawnID string, phase nodev1.SpawnPhase) {
	s.mu.Lock()
	ch, ok := s.pending[spawnID]
	s.mu.Unlock()
	if ok && (phase == nodev1.SpawnPhase_ACTIVE || phase == nodev1.SpawnPhase_ERROR) {
		select {
		case ch <- phase:
		default:
		}
	}
}

// Provision picks a node, sends StartSpawn for the (already-minted) spawn id, waits for ACTIVE,
// and binds the route. Returns the chosen node id. The caller owns id-minting + persistence.
func (s *Scheduler) Provision(ctx context.Context, id, appRef, model, name, appID, runnable, mode string, placement registry.Placement) (string, error) {
	n := s.reg.PickFor(placement)
	if n == nil {
		return "", connect.NewError(connect.CodeResourceExhausted, errors.New("no eligible node with capacity"))
	}
	ch := make(chan nodev1.SpawnPhase, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()

	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Start{Start: &nodev1.StartSpawn{
		SpawnId: id, AppRef: appRef, Model: model, Name: name, AppId: appID,
		Image: placement.Image, RunnableId: runnable, Mode: mode,
	}}}); err != nil {
		return "", connect.NewError(connect.CodeUnavailable, err)
	}
	select {
	case ph := <-ch:
		if ph != nodev1.SpawnPhase_ACTIVE {
			return "", connect.NewError(connect.CodeInternal, errors.New("spawn failed to start"))
		}
		s.rt.Bind(id, n.ID, n.Sender)
		return n.ID, nil
	case <-time.After(s.timeout):
		return "", connect.NewError(connect.CodeDeadlineExceeded, errors.New("spawn start timed out"))
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
