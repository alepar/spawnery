package registry

import (
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

type fakeSender struct{ sent []*nodev1.CPMessage }

func (f *fakeSender) Send(m *nodev1.CPMessage) error { f.sent = append(f.sent, m); return nil }

func TestAddHeartbeatPickEvict(t *testing.T) {
	r := New()
	if r.Pick() != nil {
		t.Fatal("empty registry should pick nothing")
	}
	s1, s2 := &fakeSender{}, &fakeSender{}
	r.Add(&Node{ID: "n1", Sender: s1, Max: 2, Free: 0})
	r.Add(&Node{ID: "n2", Sender: s2, Max: 4, Free: 3})
	// n1 has no free slots, n2 has 3 -> Pick returns n2 (most free).
	if n := r.Pick(); n == nil || n.ID != "n2" {
		t.Fatalf("pick: %+v", n)
	}
	r.Heartbeat("n2", 4, 0) // active=4 free=0 -> now nobody has capacity
	if r.Pick() != nil {
		t.Fatal("no capacity -> pick nil")
	}
	r.Remove("n2")
	if _, ok := r.Get("n2"); ok {
		t.Fatal("n2 should be gone")
	}
}
