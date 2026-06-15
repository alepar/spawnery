package cp

import (
	"context"
	"errors"
	"log"

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
	ForkID        string
	Generation    uint64
	Bucket        string
	NowUnixNano   int64
	DeletedAtUnix int64
	Resources     failedForkResources
}

func (s *Server) unwindFailedFork(ctx context.Context, cfg failedForkUnwind) error {
	if cfg.Resources == nil {
		log.Printf("unwindFailedFork %s: failed fork resources are not configured; leaving fork row for retry", cfg.ForkID)
		return nil
	}
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
	seq, err := s.st.Spawns().Acquire(ctx, cfg.ForkID, s.cpID, leaseID, cfg.NowUnixNano, cfg.NowUnixNano+s.claimTTL.Nanoseconds(), sp.StatusSeq)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}

	if err := cfg.Resources.RevokeForkGeneration(ctx, cfg.ForkID, cfg.Generation); err != nil {
		return err
	}
	if err := cfg.Resources.EmptyForkBucket(ctx, cfg.Bucket); err != nil {
		return err
	}
	if err := cfg.Resources.DropForkBucket(ctx, cfg.Bucket); err != nil {
		return err
	}
	if obs, ok := cfg.Resources.(failedForkRowDeleteObserver); ok {
		obs.RecordForkRowDelete(cfg.ForkID)
	}
	_, err = s.st.Spawns().MarkDeletedClaimed(ctx, cfg.ForkID, leaseID, seq, c.Generation, cfg.DeletedAtUnix)
	return err
}

func (s *Server) sweepFailedForks(ctx context.Context, resources failedForkResources) error {
	rows, err := s.st.TransferSets().ListFailedForks(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if resources == nil {
		log.Printf("sweepFailedForks: failed fork resources are not configured; leaving %d row(s) for retry", len(rows))
		return nil
	}
	for _, ts := range rows {
		forkID := ts.ForkSpawnID
		if forkID == "" {
			forkID = ts.SpawnID
		}
		now := s.now()
		if err := s.unwindFailedFork(ctx, failedForkUnwind{
			ForkID:        forkID,
			Generation:    ts.TargetGeneration,
			Bucket:        forkBucketName(forkID),
			NowUnixNano:   now.UnixNano(),
			DeletedAtUnix: now.Unix(),
			Resources:     resources,
		}); err != nil {
			return err
		}
	}
	return nil
}
