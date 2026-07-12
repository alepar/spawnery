package spawnlet

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestStorePutGetDelete(t *testing.T) {
	s := NewStore()
	s.Put(&Spawn{ID: "a", SidecarID: "s", AgentID: "g"})
	got, ok := s.Get("a")
	if !ok || got.AgentID != "g" {
		t.Fatalf("get failed: %+v ok=%v", got, ok)
	}
	s.Delete("a")
	if _, ok := s.Get("a"); ok {
		t.Fatal("expected deleted")
	}
}

func TestStoreOwnerReservationAllowsOneConcurrentCreator(t *testing.T) {
	s := NewStore()
	var winners atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, owner := range []string{"alice", "mallory"} {
		owner := owner
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.ReserveOwner("sp-1", owner, 7) {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("reservation winners = %d", winners.Load())
	}
}

func TestStoreOwnerGenerationSnapshot(t *testing.T) {
	s := NewStore()
	s.Put(&Spawn{ID: "sp-1", OwnerID: "alice", Generation: 7})
	owner, generation, ok := s.OwnerGeneration("sp-1")
	if !ok || owner != "alice" || generation != 7 {
		t.Fatalf("snapshot = %q/%d/%v", owner, generation, ok)
	}
}
