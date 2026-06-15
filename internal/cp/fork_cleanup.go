package cp

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"spawnery/internal/cp/store"
)

type failedForkResources interface {
	RevokeForkGeneration(ctx context.Context, forkID string, gen uint64) error
	EmptyForkBucket(ctx context.Context, bucket string) error
	DropForkBucket(ctx context.Context, bucket string) error
}

type failedForkRowDeleteObserver interface {
	RecordForkRowDelete(forkID string)
}

type failedForkUnwind struct {
	ForkID     string
	Generation uint64
	Bucket     string
	NowUnix    int64
	Resources  failedForkResources
}

func (s *Server) unwindFailedFork(ctx context.Context, cfg failedForkUnwind) error {
	sp, err := s.st.Spawns().Get(ctx, cfg.ForkID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c, ok, err := s.st.Spawns().LiveContainer(ctx, cfg.ForkID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	leaseID := uuid.NewString()
	seq, err := s.st.Spawns().Acquire(ctx, cfg.ForkID, s.cpID, leaseID, cfg.NowUnix, cfg.NowUnix+int64(s.claimTTL), sp.StatusSeq)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}

	if cfg.Resources != nil {
		if err := cfg.Resources.RevokeForkGeneration(ctx, cfg.ForkID, cfg.Generation); err != nil {
			return err
		}
		if err := cfg.Resources.EmptyForkBucket(ctx, cfg.Bucket); err != nil {
			return err
		}
		if err := cfg.Resources.DropForkBucket(ctx, cfg.Bucket); err != nil {
			return err
		}
	}
	if obs, ok := cfg.Resources.(failedForkRowDeleteObserver); ok {
		obs.RecordForkRowDelete(cfg.ForkID)
	}
	_, err = s.st.Spawns().MarkDeletedClaimed(ctx, cfg.ForkID, leaseID, seq, c.Generation, cfg.NowUnix)
	return err
}
