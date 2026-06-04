package node

import (
	"context"
	"io"
	"sync"
	"testing"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// fakeCPStream captures the NodeMessages the attacher sends (so a test can assert status phases) and
// serves EOF on Receive (the attacher's receive loop is not exercised here).
type fakeCPStream struct {
	mu   sync.Mutex
	sent []*nodev1.NodeMessage
}

func (f *fakeCPStream) Send(m *nodev1.NodeMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeCPStream) Receive() (*nodev1.CPMessage, error) { return nil, io.EOF }

// phasesFor returns the SpawnPhase sequence reported for spawnID, in send order.
func (f *fakeCPStream) phasesFor(spawnID string) []nodev1.SpawnPhase {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []nodev1.SpawnPhase
	for _, m := range f.sent {
		if s := m.GetStatus(); s != nil && s.SpawnId == spawnID {
			out = append(out, s.Phase)
		}
	}
	return out
}

// startSpawn must report STARTING then ERROR when the spawn cannot be created (here: a bogus app ref
// that fails manifest parsing), and must NOT register a pump or consume a capacity slot.
func TestStartSpawnCreateFailureReportsErrorNoLeak(t *testing.T) {
	mgr := spawnlet.NewManager(runtime.NewFake(), spawnlet.ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	fs := &fakeCPStream{}
	a := &attacher{cfg: Config{MaxSpawns: 2}, mgr: mgr, stream: fs, pumps: map[string]*Pump{}}

	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: "/no/such/app", Model: "m"})

	phases := fs.phasesFor("sp1")
	if len(phases) < 2 || phases[0] != nodev1.SpawnPhase_STARTING || phases[len(phases)-1] != nodev1.SpawnPhase_ERROR {
		t.Fatalf("phases = %v, want STARTING...ERROR", phases)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pumps) != 0 {
		t.Fatalf("pump map should be empty after a create failure, got %d", len(a.pumps))
	}
	if a.active != 0 {
		t.Fatalf("active count should be 0 after a create failure, got %d", a.active)
	}
}
