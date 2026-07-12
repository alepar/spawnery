package spawnlet

import "testing"

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

func TestStoreOwnerGenerationSnapshot(t *testing.T) {
	s := NewStore()
	s.Put(&Spawn{ID: "sp-1", OwnerID: "alice", Generation: 7})
	owner, generation, ok := s.OwnerGeneration("sp-1")
	if !ok || owner != "alice" || generation != 7 {
		t.Fatalf("snapshot = %q/%d/%v", owner, generation, ok)
	}
}
