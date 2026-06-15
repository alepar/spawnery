package cp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/store"
)

type failedForkResources interface {
	RevokeForkGeneration(ctx context.Context, nodeID, forkID string, gen uint64) error
	EmptyForkBucket(ctx context.Context, nodeID, forkID, bucket string) error
	DropForkBucket(ctx context.Context, nodeID, forkID, bucket string) error
}

const defaultFailedForkCleanupTimeout = 30 * time.Second

type failedForkCleanupWaiters struct {
	mu sync.Mutex
	m  map[string]chan *nodev1.FailedForkCleanupComplete
}

func newFailedForkCleanupWaiters() *failedForkCleanupWaiters {
	return &failedForkCleanupWaiters{m: map[string]chan *nodev1.FailedForkCleanupComplete{}}
}

func (w *failedForkCleanupWaiters) register(requestID string) chan *nodev1.FailedForkCleanupComplete {
	ch := make(chan *nodev1.FailedForkCleanupComplete, 1)
	w.mu.Lock()
	w.m[requestID] = ch
	w.mu.Unlock()
	return ch
}

func (w *failedForkCleanupWaiters) unregister(requestID string) {
	w.mu.Lock()
	delete(w.m, requestID)
	w.mu.Unlock()
}

func (w *failedForkCleanupWaiters) deliver(msg *nodev1.FailedForkCleanupComplete) bool {
	w.mu.Lock()
	ch, ok := w.m[msg.GetRequestId()]
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

func (s *Server) deliverFailedForkCleanupComplete(msg *nodev1.FailedForkCleanupComplete) bool {
	if s.failedForkCleanups == nil {
		return false
	}
	return s.failedForkCleanups.deliver(msg)
}

type nodeFailedForkResources struct {
	s       *Server
	timeout time.Duration
}

func (r *nodeFailedForkResources) RevokeForkGeneration(ctx context.Context, nodeID, forkID string, gen uint64) error {
	return r.send(ctx, nodeID, forkID, gen, "", nodev1.FailedForkCleanupOp_FAILED_FORK_CLEANUP_OP_REVOKE_GENERATION)
}

func (r *nodeFailedForkResources) EmptyForkBucket(ctx context.Context, nodeID, forkID, bucket string) error {
	return r.send(ctx, nodeID, forkID, 0, bucket, nodev1.FailedForkCleanupOp_FAILED_FORK_CLEANUP_OP_EMPTY_BUCKET)
}

func (r *nodeFailedForkResources) DropForkBucket(ctx context.Context, nodeID, forkID, bucket string) error {
	return r.send(ctx, nodeID, forkID, 0, bucket, nodev1.FailedForkCleanupOp_FAILED_FORK_CLEANUP_OP_DROP_BUCKET)
}

func (r *nodeFailedForkResources) send(ctx context.Context, nodeID, forkID string, gen uint64, bucket string, op nodev1.FailedForkCleanupOp) error {
	if r == nil || r.s == nil {
		return fmt.Errorf("failed fork cleanup resources are not configured")
	}
	if nodeID == "" {
		return fmt.Errorf("failed fork cleanup for %s has no target node", forkID)
	}
	n, ok := r.s.reg.Get(nodeID)
	if !ok || n.Sender == nil {
		return fmt.Errorf("failed fork cleanup node %q is not connected", nodeID)
	}
	if r.s.failedForkCleanups == nil {
		r.s.failedForkCleanups = newFailedForkCleanupWaiters()
	}
	requestID := uuid.NewString()
	ch := r.s.failedForkCleanups.register(requestID)
	defer r.s.failedForkCleanups.unregister(requestID)
	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_FailedForkCleanup{FailedForkCleanup: &nodev1.FailedForkCleanup{
		RequestId:   requestID,
		ForkSpawnId: forkID,
		Generation:  gen,
		Bucket:      bucket,
		Op:          op,
	}}}); err != nil {
		return fmt.Errorf("send failed fork cleanup %s to node %q: %w", op, nodeID, err)
	}
	timeout := r.timeout
	if timeout == 0 {
		timeout = defaultFailedForkCleanupTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("timed out waiting for failed fork cleanup %s for %s", op, forkID)
	case msg := <-ch:
		if msg.GetForkSpawnId() != forkID || msg.GetOp() != op {
			return fmt.Errorf("failed fork cleanup completion ids do not match request")
		}
		if msg.GetError() != "" {
			return fmt.Errorf("failed fork cleanup %s for %s: %s", op, forkID, msg.GetError())
		}
		return nil
	}
}

type failedForkRowDeleteObserver interface {
	RecordForkRowDelete(forkID string)
}

type failedForkUnwind struct {
	ForkID        string
	Generation    uint64
	Bucket        string
	NodeID        string
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
	expectedGen := int64(0)
	if ok {
		expectedGen = c.Generation
	}

	leaseID := uuid.NewString()
	seq, err := s.st.Spawns().Acquire(ctx, cfg.ForkID, s.cpID, leaseID, cfg.NowUnixNano, cfg.NowUnixNano+s.claimTTL.Nanoseconds(), sp.StatusSeq)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}

	nodeID := cfg.NodeID
	if nodeID == "" && ok {
		nodeID = c.NodeID
	}
	if err := cfg.Resources.RevokeForkGeneration(ctx, nodeID, cfg.ForkID, cfg.Generation); err != nil {
		return err
	}
	if err := cfg.Resources.EmptyForkBucket(ctx, nodeID, cfg.ForkID, cfg.Bucket); err != nil {
		return err
	}
	if err := cfg.Resources.DropForkBucket(ctx, nodeID, cfg.ForkID, cfg.Bucket); err != nil {
		return err
	}
	if obs, ok := cfg.Resources.(failedForkRowDeleteObserver); ok {
		obs.RecordForkRowDelete(cfg.ForkID)
	}
	_, err = s.st.Spawns().MarkDeletedClaimed(ctx, cfg.ForkID, leaseID, seq, expectedGen, cfg.DeletedAtUnix)
	return err
}

func (s *Server) sweepFailedForks(ctx context.Context, resources failedForkResources) error {
	rows, err := s.st.TransferSets().ListReclaimableForks(ctx, s.now().Add(-s.claimTTL).UnixNano())
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
			NodeID:        ts.TargetNodeID,
			NowUnixNano:   now.UnixNano(),
			DeletedAtUnix: now.Unix(),
			Resources:     resources,
		}); err != nil {
			return err
		}
	}
	return nil
}
