package cp

import (
	"context"
	"testing"

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
		ForkID:     "fork-1",
		Generation: 1,
		Bucket:     "spawnery-spawn-fork-1",
		NowUnix:    500,
		Resources:  res,
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
		ForkID:     "fork-2",
		Generation: 1,
		Bucket:     "spawnery-spawn-fork-2",
		NowUnix:    500,
		Resources:  res,
	}
	if err := s.unwindFailedFork(ctx, cfg); err != nil {
		t.Fatalf("first unwindFailedFork: %v", err)
	}
	if err := s.unwindFailedFork(ctx, cfg); err != nil {
		t.Fatalf("second unwindFailedFork must be a no-op after row deletion: %v", err)
	}
}
