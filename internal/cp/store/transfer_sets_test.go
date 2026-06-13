package store

import (
	"context"
	"errors"
	"testing"
)

func seedTransferSetSpawn(t *testing.T, st Store, id, owner string) {
	t.Helper()
	ctx := context.Background()
	if err := st.Owners().Upsert(ctx, Owner{ID: owner, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().Upsert(ctx, App{
		ID: "transfer-app", DisplayName: "Transfer", Summary: "Transfer", Tags: "",
		Visibility: "public", Listed: true, CreatorID: owner, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().UpsertVersion(ctx, AppVersion{
		AppID: "transfer-app", Version: "1.0.0", Ref: "ref", Tier: TierReviewed,
		Manifest: "{}", CreatedAt: 1,
	}, nil); err != nil {
		t.Fatal(err)
	}
	sp := Spawn{
		ID: id, OwnerID: owner, AppID: "transfer-app", AppVersion: "1.0.0", AppRef: "ref",
		Model: "m", Status: Starting, CreatedAt: 1, LastUsedAt: 1,
	}
	if err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().Create(ctx, sp, nil) }); err != nil {
		t.Fatal(err)
	}
}

func TestTransferSetCreateGetAndGenerationFencedPins(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	seedTransferSetSpawn(t, st, "sp-transfer", "alice")

	ts := TransferSet{
		ID:                            "ts-1",
		SpawnID:                       "sp-transfer",
		SourceGeneration:              3,
		TargetGeneration:              4,
		SourceNodeID:                  "node-a",
		TargetNodeID:                  "node-b",
		BaseImageDigest:               "sha256:base",
		TransferKeyCiphertextMetadata: map[string]string{"source": "sealed-source", "target": "sealed-target"},
		TransferKeyStatus:             TransferKeySourceReady,
		Status:                        TransferSetPending,
		CreatedAt:                     100,
		UpdatedAt:                     100,
	}
	if err := st.TransferSets().Create(ctx, ts); err != nil {
		t.Fatalf("Create transfer set: %v", err)
	}

	got, err := st.TransferSets().Get(ctx, "ts-1")
	if err != nil {
		t.Fatalf("Get transfer set: %v", err)
	}
	if got.SpawnID != "sp-transfer" || got.SourceGeneration != 3 || got.TargetGeneration != 4 {
		t.Fatalf("transfer set key fields = %+v", got)
	}
	if got.Status != TransferSetPending || got.TransferKeyStatus != TransferKeySourceReady {
		t.Fatalf("transfer set statuses = %s/%s", got.Status, got.TransferKeyStatus)
	}
	if got.TransferKeyCiphertextMetadata["source"] != "sealed-source" || got.TransferKeyCiphertextMetadata["target"] != "sealed-target" {
		t.Fatalf("transfer key ciphertext metadata = %+v", got.TransferKeyCiphertextMetadata)
	}

	mountPins := map[string]string{"work": "manifest-work-gen3"}
	rootfsPins := []RootfsArtifactPin{{
		ArtifactID:      "artifact-rootfs-gen3",
		ArtifactType:    "rootfs_delta",
		Generation:      3,
		Sequence:        0,
		BaseImageDigest: "sha256:base",
		Format:          "oci_layer_tar",
		ContentDigest:   "sha256:delta",
	}}
	if err := st.TransferSets().SetPins(ctx, "ts-1", 2, mountPins, rootfsPins, 101); !errors.Is(err, ErrConflict) {
		t.Fatalf("SetPins stale generation = %v, want ErrConflict", err)
	}
	if err := st.TransferSets().SetPins(ctx, "ts-1", 3, mountPins, rootfsPins, 101); err != nil {
		t.Fatalf("SetPins correct generation: %v", err)
	}
	got, err = st.TransferSets().Get(ctx, "ts-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.MountManifestPins["work"] != "manifest-work-gen3" {
		t.Fatalf("mount pins = %+v", got.MountManifestPins)
	}
	if len(got.RootfsArtifactPins) != 1 || got.RootfsArtifactPins[0].ArtifactID != "artifact-rootfs-gen3" {
		t.Fatalf("rootfs pins = %+v", got.RootfsArtifactPins)
	}
}

func TestTransferSetStatusAndKeyDeliveryUpdates(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	seedTransferSetSpawn(t, st, "sp-transfer", "alice")
	ts := TransferSet{
		ID:                "ts-2",
		SpawnID:           "sp-transfer",
		SourceGeneration:  1,
		TargetGeneration:  2,
		SourceNodeID:      "node-a",
		TargetNodeID:      "node-b",
		TransferKeyStatus: TransferKeyPending,
		Status:            TransferSetPending,
		CreatedAt:         100,
		UpdatedAt:         100,
	}
	if err := st.TransferSets().Create(ctx, ts); err != nil {
		t.Fatal(err)
	}
	if err := st.TransferSets().SetStatus(ctx, "ts-2", TransferSetCapturing, 101); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := st.TransferSets().SetTransferKeyStatus(ctx, "ts-2", TransferKeyTargetReady, 102); err != nil {
		t.Fatalf("SetTransferKeyStatus: %v", err)
	}
	got, err := st.TransferSets().Get(ctx, "ts-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TransferSetCapturing || got.TransferKeyStatus != TransferKeyTargetReady || got.UpdatedAt != 102 {
		t.Fatalf("updated transfer set = %+v", got)
	}
}

func TestTransferSetTargetNodeCanBePinnedAfterPlacement(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	seedTransferSetSpawn(t, st, "sp-transfer", "alice")
	ts := TransferSet{
		ID:                "ts-class-target",
		SpawnID:           "sp-transfer",
		SourceGeneration:  1,
		TargetGeneration:  2,
		SourceNodeID:      "source",
		TargetNodeID:      "",
		TransferKeyStatus: TransferKeySourceReady,
		Status:            TransferSetRestoring,
		CreatedAt:         100,
		UpdatedAt:         100,
	}
	if err := st.TransferSets().Create(ctx, ts); err != nil {
		t.Fatal(err)
	}
	if err := st.TransferSets().SetTargetNode(ctx, "ts-class-target", "cloud-1", 101); err != nil {
		t.Fatalf("SetTargetNode: %v", err)
	}
	got, err := st.TransferSets().Get(ctx, "ts-class-target")
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetNodeID != "cloud-1" || got.UpdatedAt != 101 {
		t.Fatalf("updated target node = %+v", got)
	}
}
