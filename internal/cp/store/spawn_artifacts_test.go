package store

import (
	"bytes"
	"context"
	"testing"
)

func TestAddAndGetArtifacts(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error {
		return tx.Spawns().Create(ctx, newSpawn("sp1"), []Mount{{Name: "main", BackendURI: "scratch"}})
	})

	arts := []Artifact{
		{ArtifactID: "a1", Inline: []byte("hello"), ContentType: 1, TargetContainer: 1, DestPath: "skills/x", Mode: 0o600},
		{ArtifactID: "a2", Sensitive: true, EnvVarName: "TOKEN", ContentType: 1, TargetContainer: 1, DestPath: "mcp/y"},
	}
	if err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().AddArtifacts(ctx, "sp1", arts) }); err != nil {
		t.Fatalf("AddArtifacts: %v", err)
	}
	got, err := st.Spawns().GetArtifacts(ctx, "sp1")
	if err != nil {
		t.Fatalf("GetArtifacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ArtifactID != "a1" || !bytes.Equal(got[0].Inline, []byte("hello")) {
		t.Fatalf("a1 = %+v", got[0])
	}
	if got[1].ArtifactID != "a2" || !got[1].Sensitive || got[1].EnvVarName != "TOKEN" || len(got[1].Inline) != 0 {
		t.Fatalf("a2 = %+v", got[1])
	}
}

func TestGetArtifactsEmpty(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error {
		return tx.Spawns().Create(ctx, newSpawn("sp1"), []Mount{{Name: "main", BackendURI: "scratch"}})
	})
	got, err := st.Spawns().GetArtifacts(ctx, "sp1")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want empty", got, err)
	}
}
