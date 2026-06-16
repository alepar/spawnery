package node

import (
	"context"
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

func TestStartSpawnThreadsMountBindingsIntoCreate(t *testing.T) {
	fs := &fakeCPStream{}
	mgr := newGooseManager(t, &scriptedPodBackend{script: scriptGooseDieOnPrompt})
	a := newAttacher(mgr, fs)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: "sp-bound-mount",
		AppRef:  writeNodeJournalApp(t),
		Model:   "m",
		Mounts: []*nodev1.MountBinding{{
			Name:       "main",
			BackendUri: "scratch:",
		}},
	})

	st := fs.lastStatusFor("sp-bound-mount")
	if st == nil {
		t.Fatal("missing spawn status")
	}
	if st.Phase != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("final phase = %s, want ACTIVE", st.Phase)
	}
	bindings, ok := mgr.MountBindings("sp-bound-mount")
	if !ok {
		t.Fatal("spawn manager did not retain mount bindings")
	}
	if len(bindings) != 1 || bindings[0].Name != "main" || bindings[0].BackendURI != "scratch:" {
		t.Fatalf("mount bindings = %+v, want scratch main binding", bindings)
	}
}
