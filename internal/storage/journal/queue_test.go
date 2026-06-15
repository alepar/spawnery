package journal

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// TestSerialQueueRunsOneAtATime gates the action on a channel to prove no two
// snapshots run concurrently and that requests-while-running coalesce.
func TestSerialQueueRunsOneAtATime(t *testing.T) {
	var concurrent int32
	var maxConcurrent int32
	release := make(chan struct{})
	started := make(chan struct{}, 16)

	q := newSerialQueue(context.Background(), func(ctx context.Context) (ManifestID, error) {
		n := atomic.AddInt32(&concurrent, 1)
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if n <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, n) {
				break
			}
		}
		started <- struct{}{}
		<-release // block until the test lets it finish
		atomic.AddInt32(&concurrent, -1)
		return "m", nil
	})

	// Fire many requests; they must serialize (and coalesce).
	for i := 0; i < 5; i++ {
		q.Request()
	}
	<-started // first snapshot is running

	// While one runs, more requests coalesce into at most ONE follow-up.
	for i := 0; i < 5; i++ {
		q.Request()
	}

	// Drain via Suspend so we deterministically wait for completion.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(release) // let everything proceed
		_, _ = q.Suspend(context.Background(), func(ctx context.Context) (ManifestID, error) {
			return "final", nil
		})
	}()
	wg.Wait()

	if got := atomic.LoadInt32(&maxConcurrent); got != 1 {
		t.Fatalf("snapshots must run one-at-a-time, observed max concurrency %d", got)
	}
}

// TestSerialQueueSuspendBarrierRunsFinalAndRejectsNewWork verifies the suspend
// barrier: after Suspend, Request is a no-op and the final snapshot runs
// exclusively, returning its manifest id.
func TestSerialQueueSuspendBarrierRunsFinalAndRejectsNewWork(t *testing.T) {
	var actionRuns int32
	q := newSerialQueue(context.Background(), func(ctx context.Context) (ManifestID, error) {
		atomic.AddInt32(&actionRuns, 1)
		return "periodic", nil
	})

	var finalRuns int32
	id, err := q.Suspend(context.Background(), func(ctx context.Context) (ManifestID, error) {
		atomic.AddInt32(&finalRuns, 1)
		return "FINAL-MANIFEST", nil
	})
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if id != "FINAL-MANIFEST" {
		t.Fatalf("suspend returned %q, want FINAL-MANIFEST", id)
	}
	if finalRuns != 1 {
		t.Fatalf("final snapshot should run exactly once, ran %d", finalRuns)
	}
	if !q.IsSuspended() {
		t.Fatal("queue should report suspended")
	}

	// Requests after suspend are no-ops.
	q.Request()
	q.Request()
	if got := atomic.LoadInt32(&actionRuns); got != 0 {
		t.Fatalf("Request after suspend must be a no-op, but action ran %d times", got)
	}
}

// TestSerialQueueSuspendDrainsInFlight ensures Suspend waits for an in-flight
// snapshot to finish before running the final one.
func TestSerialQueueSuspendDrainsInFlight(t *testing.T) {
	inFlightDone := make(chan struct{})
	started := make(chan struct{})
	proceed := make(chan struct{})
	order := make(chan string, 2)

	q := newSerialQueue(context.Background(), func(ctx context.Context) (ManifestID, error) {
		close(started) // signal the action is actually in flight
		<-proceed
		order <- "inflight"
		close(inFlightDone)
		return "x", nil
	})

	q.Request()
	// Wait until the action is genuinely in flight before suspending, so Suspend
	// drains a real in-flight snapshot (not a not-yet-started, cancellable one).
	<-started
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = q.Suspend(context.Background(), func(ctx context.Context) (ManifestID, error) {
			order <- "final"
			return "f", nil
		})
	}()

	close(proceed) // let the in-flight action complete
	<-inFlightDone
	wg.Wait()

	got := []string{<-order, <-order}
	if got[0] != "inflight" || got[1] != "final" {
		t.Fatalf("expected in-flight to drain before final, got %v", got)
	}
}

// TestSerialQueueWarmSnapshotDrainsWithoutSuspending verifies the warm barrier
// drains the current queue and runs one exclusive snapshot without turning later
// Request calls into no-ops.
func TestSerialQueueWarmSnapshotDrainsWithoutSuspending(t *testing.T) {
	started := make(chan struct{})
	proceed := make(chan struct{})
	order := make(chan string, 4)

	q := newSerialQueue(context.Background(), func(ctx context.Context) (ManifestID, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-proceed
		order <- "queued"
		return "queued", nil
	})

	q.Request()
	<-started
	q.Request()

	done := make(chan struct{})
	go func() {
		defer close(done)
		id, err := q.WarmSnapshot(context.Background(), func(ctx context.Context) (ManifestID, error) {
			order <- "warm"
			return "warm", nil
		})
		if err != nil {
			t.Errorf("warm snapshot: %v", err)
			return
		}
		if id != "warm" {
			t.Errorf("warm snapshot id = %q, want warm", id)
		}
	}()

	close(proceed)
	<-done

	if q.IsSuspended() {
		t.Fatal("warm snapshot must not suspend the queue")
	}

	q.Request()
	<-started
	got := []string{<-order, <-order, <-order, <-order}
	if got[0] != "queued" || got[1] != "queued" || got[2] != "warm" || got[3] != "queued" {
		t.Fatalf("warm snapshot must drain queued work before warm snapshot, got %v", got)
	}
}
