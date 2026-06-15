package cp

import (
	"context"
	"errors"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

type recordingForkResources struct {
	ops []string
}

func (r *recordingForkResources) RecordForkRowDelete(forkID string) {
	r.ops = append(r.ops, "delete-row:"+forkID)
}

func (r *recordingForkResources) RevokeForkGeneration(ctx context.Context, nodeID, forkID string, gen uint64) error {
	_ = ctx
	_ = nodeID
	_ = gen
	r.ops = append(r.ops, "revoke-key:"+forkID)
	return nil
}

func (r *recordingForkResources) EmptyForkBucket(ctx context.Context, nodeID, forkID, bucket string) error {
	_ = ctx
	_ = nodeID
	_ = forkID
	r.ops = append(r.ops, "empty-bucket:"+bucket)
	return nil
}

func (r *recordingForkResources) DropForkBucket(ctx context.Context, nodeID, forkID, bucket string) error {
	_ = ctx
	_ = nodeID
	_ = forkID
	r.ops = append(r.ops, "drop-bucket:"+bucket)
	return nil
}

func newForkCleanupTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, _, _ := newTestServer(t)
	return s, s.st
}

func TestNewServerWiresFailedForkResourcesByDefault(t *testing.T) {
	s, _, _ := newTestServer(t)
	if s.failedForkResources == nil {
		t.Fatal("NewServer must wire production failed-fork resources by default")
	}
	if _, ok := s.failedForkResources.(*nodeFailedForkResources); !ok {
		t.Fatalf("default failedForkResources = %T, want node-backed production resources", s.failedForkResources)
	}
}

func seedPartialFork(t *testing.T, st store.Store, forkID string) {
	t.Helper()
	makeSpawn(t, &Server{st: st}, forkID, "alice")
}

func TestUnwindFailedForkOrderingAndRowLast(t *testing.T) {
	s, st := newForkCleanupTestServer(t)
	ctx := context.Background()
	seedPartialFork(t, st, "fork-1")

	res := &recordingForkResources{}
	if err := s.unwindFailedFork(ctx, failedForkUnwind{
		ForkID:        "fork-1",
		Generation:    1,
		Bucket:        "spawnery-spawn-fork-1",
		NowUnixNano:   500_000_000_000,
		DeletedAtUnix: 500,
		Resources:     res,
	}); err != nil {
		t.Fatalf("unwindFailedFork: %v", err)
	}

	want := []string{
		"revoke-key:fork-1",
		"empty-bucket:spawnery-spawn-fork-1",
		"drop-bucket:spawnery-spawn-fork-1",
		"delete-row:fork-1",
	}
	if got := res.ops; len(got) != len(want) {
		t.Fatalf("ops=%v want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ops=%v want %v", got, want)
			}
		}
	}
	if _, err := st.Spawns().Get(ctx, "fork-1"); err == nil {
		t.Fatal("fork row must be deleted last and then hidden from Get")
	}
}

func TestUnwindFailedForkIsRedrivableAfterPartialBucketCleanup(t *testing.T) {
	s, st := newForkCleanupTestServer(t)
	ctx := context.Background()
	seedPartialFork(t, st, "fork-2")

	res := &recordingForkResources{}
	cfg := failedForkUnwind{
		ForkID:        "fork-2",
		Generation:    1,
		Bucket:        "spawnery-spawn-fork-2",
		NowUnixNano:   500_000_000_000,
		DeletedAtUnix: 500,
		Resources:     res,
	}
	if err := s.unwindFailedFork(ctx, cfg); err != nil {
		t.Fatalf("first unwindFailedFork: %v", err)
	}
	if err := s.unwindFailedFork(ctx, cfg); err != nil {
		t.Fatalf("second unwindFailedFork must be a no-op after row deletion: %v", err)
	}
}

func TestUnwindFailedForkWithNilResourcesLeavesForkForRetry(t *testing.T) {
	s, st := newForkCleanupTestServer(t)
	ctx := context.Background()
	seedPartialFork(t, st, "fork-nil-resources")

	if err := s.unwindFailedFork(ctx, failedForkUnwind{
		ForkID:        "fork-nil-resources",
		Generation:    1,
		Bucket:        "spawnery-spawn-fork-nil-resources",
		NowUnixNano:   500_000_000_000,
		DeletedAtUnix: 500,
	}); err != nil {
		t.Fatalf("unwindFailedFork: %v", err)
	}
	if _, err := st.Spawns().Get(ctx, "fork-nil-resources"); err != nil {
		t.Fatalf("nil resources must leave fork row visible for retry, Get err=%v", err)
	}
}

func TestSweepFailedForksIsIdempotent(t *testing.T) {
	s, st := newForkCleanupTestServer(t)
	ctx := context.Background()
	s.now = func() time.Time { return time.Unix(500, 0) }
	seedPartialFork(t, st, "source-1")
	seedPartialFork(t, st, "fork-sweep")
	if err := st.TransferSets().Create(ctx, store.TransferSet{
		ID:                "ts-sweep",
		Kind:              store.TransferSetFork,
		SpawnID:           "fork-sweep",
		SourceSpawnID:     "source-1",
		ForkSpawnID:       "fork-sweep",
		SourceGeneration:  3,
		TargetGeneration:  1,
		SourceNodeID:      "source-node",
		TargetNodeID:      "target-node",
		TransferKeyStatus: store.TransferKeyPending,
		Status:            store.TransferSetFailed,
		CreatedAt:         100,
		UpdatedAt:         100,
	}); err != nil {
		t.Fatalf("Create failed fork transfer set: %v", err)
	}

	res := &recordingForkResources{}
	if err := s.sweepFailedForks(ctx, res); err != nil {
		t.Fatalf("sweepFailedForks first: %v", err)
	}
	want := []string{
		"revoke-key:fork-sweep",
		"empty-bucket:spawnery-spawn-fork-sweep",
		"drop-bucket:spawnery-spawn-fork-sweep",
		"delete-row:fork-sweep",
	}
	if got := res.ops; len(got) != len(want) {
		t.Fatalf("ops=%v want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ops=%v want %v", got, want)
			}
		}
	}
	if _, err := st.Spawns().Get(ctx, "fork-sweep"); err == nil {
		t.Fatal("sweep must hide the failed fork row after ordered unwind")
	}

	if err := s.sweepFailedForks(ctx, res); err != nil {
		t.Fatalf("sweepFailedForks second: %v", err)
	}
	if got := res.ops; len(got) != len(want) {
		t.Fatalf("second sweep must be a no-op, ops=%v want %v", got, want)
	}
}

func TestSweepFailedForksReclaimsStaleRestoringForks(t *testing.T) {
	s, st := newForkCleanupTestServer(t)
	ctx := context.Background()
	s.claimTTL = time.Second
	s.now = func() time.Time { return time.Unix(0, 5*time.Second.Nanoseconds()) }
	seedPartialFork(t, st, "source-stale-restoring")
	seedPartialFork(t, st, "fork-stale-restoring")
	if err := st.TransferSets().Create(ctx, store.TransferSet{
		ID:                "ts-stale-restoring",
		Kind:              store.TransferSetFork,
		SpawnID:           "fork-stale-restoring",
		SourceSpawnID:     "source-stale-restoring",
		ForkSpawnID:       "fork-stale-restoring",
		SourceGeneration:  3,
		TargetGeneration:  1,
		SourceNodeID:      "source-node",
		TargetNodeID:      "target-node",
		TransferKeyStatus: store.TransferKeyPending,
		Status:            store.TransferSetRestoring,
		CreatedAt:         100,
		UpdatedAt:         int64(time.Second),
	}); err != nil {
		t.Fatalf("Create stale restoring fork transfer set: %v", err)
	}

	res := &recordingForkResources{}
	if err := s.sweepFailedForks(ctx, res); err != nil {
		t.Fatalf("sweepFailedForks: %v", err)
	}
	want := []string{
		"revoke-key:fork-stale-restoring",
		"empty-bucket:spawnery-spawn-fork-stale-restoring",
		"drop-bucket:spawnery-spawn-fork-stale-restoring",
		"delete-row:fork-stale-restoring",
	}
	if got := res.ops; len(got) != len(want) {
		t.Fatalf("ops=%v want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ops=%v want %v", got, want)
			}
		}
	}
	if _, err := st.Spawns().Get(ctx, "fork-stale-restoring"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale restoring fork should be deleted after cleanup, err=%v", err)
	}
}

type cleanupAckSender struct {
	capSender
	s *Server
}

func (c *cleanupAckSender) Send(m *nodev1.CPMessage) error {
	if err := c.capSender.Send(m); err != nil {
		return err
	}
	if cmd := m.GetFailedForkCleanup(); cmd != nil {
		c.s.deliverFailedForkCleanupComplete(&nodev1.FailedForkCleanupComplete{
			RequestId:   cmd.GetRequestId(),
			ForkSpawnId: cmd.GetForkSpawnId(),
			Op:          cmd.GetOp(),
		})
	}
	return nil
}

func (c *cleanupAckSender) cleanupOps() []nodev1.FailedForkCleanupOp {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []nodev1.FailedForkCleanupOp
	for _, msg := range c.sent {
		if cmd := msg.GetFailedForkCleanup(); cmd != nil {
			out = append(out, cmd.GetOp())
		}
	}
	return out
}

func TestSweepFailedForksUsesDefaultNodeResources(t *testing.T) {
	s, reg, _ := newTestServer(t)
	st := s.st
	ctx := context.Background()
	s.now = func() time.Time { return time.Unix(500, 0) }
	seedPartialFork(t, st, "source-default-sweep")
	seedPartialFork(t, st, "fork-default-sweep")
	sender := &cleanupAckSender{s: s}
	reg.Add(&registry.Node{
		ID: "target-node", Sender: sender, Max: 1, Free: 1, Class: "cloud",
		Images: []string{"img:agent"}, DiskFreeBytes: 1_000_000,
	})
	if err := st.TransferSets().Create(ctx, store.TransferSet{
		ID:                "ts-default-sweep",
		Kind:              store.TransferSetFork,
		SpawnID:           "fork-default-sweep",
		SourceSpawnID:     "source-default-sweep",
		ForkSpawnID:       "fork-default-sweep",
		SourceGeneration:  3,
		TargetGeneration:  1,
		SourceNodeID:      "source-node",
		TargetNodeID:      "target-node",
		TransferKeyStatus: store.TransferKeyPending,
		Status:            store.TransferSetFailed,
		CreatedAt:         100,
		UpdatedAt:         100,
	}); err != nil {
		t.Fatalf("Create failed fork transfer set: %v", err)
	}

	if err := s.sweepFailedForks(ctx, s.failedForkResources); err != nil {
		t.Fatalf("sweepFailedForks: %v", err)
	}
	wantOps := []nodev1.FailedForkCleanupOp{
		nodev1.FailedForkCleanupOp_FAILED_FORK_CLEANUP_OP_REVOKE_GENERATION,
		nodev1.FailedForkCleanupOp_FAILED_FORK_CLEANUP_OP_EMPTY_BUCKET,
		nodev1.FailedForkCleanupOp_FAILED_FORK_CLEANUP_OP_DROP_BUCKET,
	}
	if got := sender.cleanupOps(); len(got) != len(wantOps) {
		t.Fatalf("cleanup ops = %v want %v", got, wantOps)
	} else {
		for i := range wantOps {
			if got[i] != wantOps[i] {
				t.Fatalf("cleanup ops = %v want %v", got, wantOps)
			}
		}
	}
	if _, err := st.Spawns().Get(ctx, "fork-default-sweep"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("default failed-fork resources must let sweep hide row, err=%v", err)
	}
	rows, err := st.TransferSets().ListFailedForks(ctx)
	if err != nil {
		t.Fatalf("ListFailedForks: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("failed fork rows should not stay visible after default-resource sweep: %+v", rows)
	}
}

func TestStartReconcilerSweepsFailedForks(t *testing.T) {
	s, st := newForkCleanupTestServer(t)
	ctx := context.Background()
	s.reconcileInterval = time.Hour
	s.now = func() time.Time { return time.Unix(500, 0) }
	seedPartialFork(t, st, "source-reconcile")
	seedPartialFork(t, st, "fork-reconcile")
	if err := st.TransferSets().Create(ctx, store.TransferSet{
		ID:                "ts-reconcile",
		Kind:              store.TransferSetFork,
		SpawnID:           "fork-reconcile",
		SourceSpawnID:     "source-reconcile",
		ForkSpawnID:       "fork-reconcile",
		SourceGeneration:  3,
		TargetGeneration:  1,
		SourceNodeID:      "source-node",
		TargetNodeID:      "target-node",
		TransferKeyStatus: store.TransferKeyPending,
		Status:            store.TransferSetFailed,
		CreatedAt:         100,
		UpdatedAt:         100,
	}); err != nil {
		t.Fatalf("Create failed fork transfer set: %v", err)
	}
	res := &recordingForkResources{}
	s.failedForkResources = res

	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartReconciler(loopCtx)

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		_, err := st.Spawns().Get(ctx, "fork-reconcile")
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			t.Fatalf("Get fork-reconcile: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("StartReconciler did not sweep failed fork before first ticker interval, ops=%v", res.ops)
		}
		time.Sleep(5 * time.Millisecond)
	}
	want := []string{
		"revoke-key:fork-reconcile",
		"empty-bucket:spawnery-spawn-fork-reconcile",
		"drop-bucket:spawnery-spawn-fork-reconcile",
		"delete-row:fork-reconcile",
	}
	if got := res.ops; len(got) != len(want) {
		t.Fatalf("ops=%v want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ops=%v want %v", got, want)
			}
		}
	}
}

func TestUnwindFailedForkUsesNanoClaimDeadlineAndSecondDeletedAt(t *testing.T) {
	ctx := context.Background()
	nowNS := int64(500_000_000_000)
	deletedAtUnix := int64(500)
	ttl := 2 * time.Second
	spawns := &capturingForkUnwindSpawnRepo{}
	s := &Server{
		st:       capturingForkUnwindStore{spawns: spawns},
		cpID:     "cp-test",
		claimTTL: ttl,
	}

	if err := s.unwindFailedFork(ctx, failedForkUnwind{
		ForkID:        "fork-units",
		Generation:    1,
		Bucket:        "spawnery-spawn-fork-units",
		NowUnixNano:   nowNS,
		DeletedAtUnix: deletedAtUnix,
		Resources:     &recordingForkResources{},
	}); err != nil {
		t.Fatalf("unwindFailedFork: %v", err)
	}
	if spawns.acquireNowTS != nowNS {
		t.Fatalf("Acquire nowTS=%d want UnixNano %d", spawns.acquireNowTS, nowNS)
	}
	if want := nowNS + ttl.Nanoseconds(); spawns.acquireDeadlineTS != want {
		t.Fatalf("Acquire deadlineTS=%d want UnixNano deadline %d", spawns.acquireDeadlineTS, want)
	}
	if spawns.markDeletedTS != deletedAtUnix {
		t.Fatalf("MarkDeletedClaimed ts=%d want Unix seconds %d", spawns.markDeletedTS, deletedAtUnix)
	}
}

type capturingForkUnwindStore struct {
	store.Store
	spawns *capturingForkUnwindSpawnRepo
}

func (s capturingForkUnwindStore) Spawns() store.SpawnRepo {
	return s.spawns
}

type capturingForkUnwindSpawnRepo struct {
	store.SpawnRepo
	acquireNowTS      int64
	acquireDeadlineTS int64
	markDeletedTS     int64
}

func (r *capturingForkUnwindSpawnRepo) Get(context.Context, string) (store.Spawn, error) {
	return store.Spawn{ID: "fork-units", StatusSeq: 41}, nil
}

func (r *capturingForkUnwindSpawnRepo) LiveContainer(context.Context, string) (store.Container, bool, error) {
	return store.Container{SpawnID: "fork-units", Generation: 7}, true, nil
}

func (r *capturingForkUnwindSpawnRepo) Acquire(_ context.Context, _ string, _, _ string, nowTS, deadlineTS, expectedSeq int64) (int64, error) {
	r.acquireNowTS = nowTS
	r.acquireDeadlineTS = deadlineTS
	return expectedSeq + 1, nil
}

func (r *capturingForkUnwindSpawnRepo) MarkDeletedClaimed(_ context.Context, _ string, _ string, expectedSeq, _ int64, ts int64) (int64, error) {
	r.markDeletedTS = ts
	return expectedSeq + 1, nil
}
