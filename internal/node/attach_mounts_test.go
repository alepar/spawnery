package node

import (
	"context"
	"testing"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
)

func TestMountBindingsFromProtoMapsAllFiveFieldsAndSkipsNil(t *testing.T) {
	t.Parallel()

	in := []*nodev1.MountBinding{
		{
			Name:               "main",
			BackendUri:         "github:o/r",
			CredentialSecretId: "sec-9",
			CreateIfMissing:    true,
			RepositoryId:       "7",
		},
		nil, // must be skipped
	}
	out := mountBindingsFromProto(in)

	if len(out) != 1 {
		t.Fatalf("mountBindingsFromProto returned %d entries, want 1 (nil skipped)", len(out))
	}
	want := spawnlet.MountBinding{
		Name:               "main",
		BackendURI:         "github:o/r",
		CredentialSecretID: "sec-9",
		CreateIfMissing:    true,
		RepositoryID:       "7",
	}
	got := out[0]
	if got != want {
		t.Fatalf("mountBindingsFromProto[0] = %+v, want %+v", got, want)
	}
}

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
