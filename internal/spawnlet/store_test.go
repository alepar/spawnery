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
