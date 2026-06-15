package cp

import (
	"context"
	"testing"
	"time"

	"spawnery/internal/cp/store"
)

type recordingForkResources struct {
	ops []string
}

func (r *recordingForkResources) RecordForkRowDelete(forkID string) {
	r.ops = append(r.ops, "delete-row:"+forkID)
}

func (r *recordingForkResources) RevokeForkGeneration(ctx context.Context, forkID string, gen uint64) error {
	_ = ctx
	_ = gen
	r.ops = append(r.ops, "revoke-key:"+forkID)
	return nil
}

func (r *recordingForkResources) EmptyForkBucket(ctx context.Context, bucket string) error {
	_ = ctx
	r.ops = append(r.ops, "empty-bucket:"+bucket)
	return nil
}

func (r *recordingForkResources) DropForkBucket(ctx context.Context, bucket string) error {
	_ = ctx
	r.ops = append(r.ops, "drop-bucket:"+bucket)
	return nil
}

func newForkCleanupTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, _, _ := newTestServer(t)
	return s, s.st
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
