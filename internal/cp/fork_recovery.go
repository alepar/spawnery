package cp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/store"
)

type forkPauseController interface {
	UnpauseIfPaused(ctx context.Context, spawnID string, generation int64) error
}

type forkUnpauseWaiters struct {
	mu sync.Mutex
	m  map[string]chan *nodev1.UnpauseIfPausedComplete
}

func newForkUnpauseWaiters() *forkUnpauseWaiters {
	return &forkUnpauseWaiters{m: map[string]chan *nodev1.UnpauseIfPausedComplete{}}
}

func forkUnpauseKey(spawnID string, generation int64) string {
	return fmt.Sprintf("%s:%d", spawnID, generation)
}

func (w *forkUnpauseWaiters) register(spawnID string, generation int64) chan *nodev1.UnpauseIfPausedComplete {
	ch := make(chan *nodev1.UnpauseIfPausedComplete, 1)
	w.mu.Lock()
	w.m[forkUnpauseKey(spawnID, generation)] = ch
	w.mu.Unlock()
	return ch
}

func (w *forkUnpauseWaiters) unregister(spawnID string, generation int64) {
	w.mu.Lock()
	delete(w.m, forkUnpauseKey(spawnID, generation))
	w.mu.Unlock()
}

func (w *forkUnpauseWaiters) deliver(msg *nodev1.UnpauseIfPausedComplete) bool {
	w.mu.Lock()
	ch, ok := w.m[forkUnpauseKey(msg.GetSpawnId(), int64(msg.GetGeneration()))]
	w.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}

func (s *Server) deliverUnpauseIfPausedComplete(msg *nodev1.UnpauseIfPausedComplete) bool {
	if s.forkUnpauses == nil {
		return false
	}
	return s.forkUnpauses.deliver(msg)
}

type nodeForkPauseController struct {
	s       *Server
	timeout time.Duration
}

func (s *Server) forkPauseController() forkPauseController {
	return &nodeForkPauseController{s: s, timeout: defaultForkMaterializeTimeout}
}

func (p *nodeForkPauseController) UnpauseIfPaused(ctx context.Context, spawnID string, generation int64) error {
	c, ok, err := p.s.st.Spawns().LiveContainer(ctx, spawnID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if generation != 0 && c.Generation != generation {
		return nil
	}
	n, ok := p.s.reg.Get(c.NodeID)
	if !ok || n.Sender == nil {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("source node %q is not connected", c.NodeID))
	}
	if p.s.forkUnpauses == nil {
		p.s.forkUnpauses = newForkUnpauseWaiters()
	}
	ch := p.s.forkUnpauses.register(spawnID, generation)
	defer p.s.forkUnpauses.unregister(spawnID, generation)
	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_UnpauseIfPaused{UnpauseIfPaused: &nodev1.UnpauseIfPaused{
		SpawnId:    spawnID,
		Generation: uint64(generation),
	}}}); err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("send unpause to node %q: %w", c.NodeID, err))
	}
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("timed out waiting for unpause of %s gen %d", spawnID, generation))
	case msg := <-ch:
		if msg.GetError() != "" {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("unpause source: %s", msg.GetError()))
		}
		return nil
	}
}

func (s *Server) recoverForkingSources(ctx context.Context, pause forkPauseController) error {
	nowTS := s.now().UnixNano()
	rows, err := s.st.Spawns().ListRecoverableForking(ctx, nowTS)
	if err != nil {
		return fmt.Errorf("list recoverable forking sources: %w", err)
	}
	for _, sp := range rows {
		if err := s.recoverForkingSource(ctx, sp, pause, nowTS); err != nil {
			log.Printf("recover forking source %s: %v", sp.ID, err)
		}
	}
	return nil
}

func (s *Server) recoverForkingSource(ctx context.Context, sp store.Spawn, pause forkPauseController, nowTS int64) error {
	c, ok, err := s.st.Spawns().LiveContainer(ctx, sp.ID)
	if err != nil {
		return err
	}
	if !ok {
		if _, err := s.st.Spawns().MarkForkingLost(ctx, sp.ID, sp.StatusSeq); errors.Is(err, store.ErrConflict) {
			return nil
		} else if err != nil {
			return err
		}
		return nil
	}

	leaseID := uuid.NewString()
	deadlineTS := s.now().Add(s.claimTTL).UnixNano()
	seq, err := s.st.Spawns().AcquireForkingRecovery(ctx, sp.ID, s.cpID, leaseID, nowTS, deadlineTS, sp.StatusSeq)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := pause.UnpauseIfPaused(ctx, sp.ID, c.Generation); err != nil {
		code := connect.CodeOf(err)
		if code == connect.CodeFailedPrecondition || code == connect.CodeUnavailable {
			if _, lerr := s.st.Spawns().MarkForkingLost(ctx, sp.ID, seq); errors.Is(lerr, store.ErrConflict) {
				return nil
			} else if lerr != nil {
				return lerr
			}
			return nil
		}
		return err
	}
	if _, err := s.st.Spawns().TransitionForkingRecovered(ctx, sp.ID, leaseID, seq, c.Generation); errors.Is(err, store.ErrConflict) {
		return nil
	} else if err != nil {
		return err
	}
	_ = s.st.Spawns().Release(context.WithoutCancel(ctx), sp.ID, leaseID)
	return nil
}
