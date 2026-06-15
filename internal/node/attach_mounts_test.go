package node

import (
	"context"
	"strings"
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

func TestStartSpawnThreadsMountBindingsIntoCreate(t *testing.T) {
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, &scriptedPodBackend{script: scriptGooseDieOnPrompt}), fs)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: "sp-github-mount",
		AppRef:  writeNodeApp(t),
		Model:   "m",
		Mounts: []*nodev1.MountBinding{{
			Name:       "main",
			BackendUri: "github:owner/repo",
		}},
	})

	st := fs.lastStatusFor("sp-github-mount")
	if st == nil {
		t.Fatal("missing spawn status")
	}
	if st.Phase != nodev1.SpawnPhase_ERROR {
		t.Fatalf("final phase = %s, want ERROR", st.Phase)
	}
	if !strings.Contains(strings.ToLower(st.Detail), "unsupported backend") {
		t.Fatalf("status detail = %q, want unsupported backend detail", st.Detail)
	}
}
