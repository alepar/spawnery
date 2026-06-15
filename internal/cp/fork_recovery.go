package cp

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"spawnery/internal/cp/store"
)

type forkPauseController interface {
	UnpauseIfPaused(ctx context.Context, spawnID string, generation int64) error
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
