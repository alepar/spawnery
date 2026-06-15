package contract

import (
	"testing"

	"google.golang.org/protobuf/proto"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
)

func TestNodeContractFields(t *testing.T) {
	// generation threaded onto every CP->node command + onto SpawnStatus
	start := &nodev1.StartSpawn{
		SpawnId: "sp1", AppRef: "ref", DataRef: "", Model: "m",
		Generation:             7,
		Mounts:                 []*nodev1.MountBinding{{Name: "main", BackendUri: "managed:repo"}},
		RootfsSourceGeneration: 6,
		RootfsArtifacts: []*nodev1.RootfsArtifact{{
			ArtifactId: "artifact-rootfs-gen6", Generation: 6, Sequence: 1, BaseImageDigest: "agent@sha256:base",
			Format: "oci_layout",
		}},
	}
	_ = &nodev1.StopSpawn{SpawnId: "sp1", Generation: 7}
	_ = &nodev1.Suspend{SpawnId: "sp1", Generation: 7, CaptureRootfsArtifact: true}
	_ = &nodev1.SessionOpen{SpawnId: "sp1", Generation: 7}
	_ = &nodev1.SessionClose{SpawnId: "sp1", Generation: 7}
	_ = &nodev1.SpawnStatus{SpawnId: "sp1", Phase: nodev1.SpawnPhase_SUSPENDED, Generation: 7}

	// new CP->node Suspend command variant; new node->CP SuspendComplete with per-mount markers
	_ = &nodev1.CPMessage{Msg: &nodev1.CPMessage_Suspend{
		Suspend: &nodev1.Suspend{SpawnId: "sp1", Generation: 7}}}
	_ = &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SuspendComplete{
		SuspendComplete: &nodev1.SuspendComplete{
			SpawnId: "sp1", Generation: 7,
			Markers: []*nodev1.MountMarker{{Name: "main", Marker: "spawnery-suspend/sp1/7"}},
			RootfsArtifacts: []*nodev1.RootfsArtifact{{
				ArtifactId: "artifact-rootfs-gen7", Generation: 7, Sequence: 1, BaseImageDigest: "agent@sha256:base",
			}},
		}}}

	// node inventory on Register + Heartbeat
	_ = &nodev1.Register{NodeId: "n1", Running: []*nodev1.RunningSpawn{
		{SpawnId: "sp1", Generation: 7, Phase: nodev1.SpawnPhase_ACTIVE}}}
	_ = &nodev1.Heartbeat{Running: []*nodev1.RunningSpawn{
		{SpawnId: "sp1", Generation: 7, Phase: nodev1.SpawnPhase_ACTIVE}}}

	// round-trip proves the wire actually encodes the new fields
	b, err := proto.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	var got nodev1.StartSpawn
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Generation != 7 || len(got.Mounts) != 1 || got.Mounts[0].Name != "main" ||
		got.RootfsSourceGeneration != 6 || len(got.RootfsArtifacts) != 1 ||
		got.RootfsArtifacts[0].ArtifactId != "artifact-rootfs-gen6" {
		t.Fatalf("round-trip lost fields: %+v", &got)
	}

	// round-trip a nested-repeated field (SuspendComplete.Markers) — most likely to mis-serialize
	sc := &nodev1.SuspendComplete{SpawnId: "sp1", Generation: 7,
		Markers: []*nodev1.MountMarker{{Name: "main", Marker: "spawnery-suspend/sp1/7"}},
		RootfsArtifacts: []*nodev1.RootfsArtifact{{
			ArtifactId: "artifact-rootfs-gen7", Generation: 7, Sequence: 1, BaseImageDigest: "agent@sha256:base",
		}}}
	scb, err := proto.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	var gotSC nodev1.SuspendComplete
	if err := proto.Unmarshal(scb, &gotSC); err != nil {
		t.Fatal(err)
	}
	if gotSC.Generation != 7 || len(gotSC.Markers) != 1 || gotSC.Markers[0].Marker != "spawnery-suspend/sp1/7" ||
		len(gotSC.RootfsArtifacts) != 1 || gotSC.RootfsArtifacts[0].ArtifactId != "artifact-rootfs-gen7" {
		t.Fatalf("SuspendComplete round-trip lost fields: %+v", &gotSC)
	}
}

func TestNodeForkMessagesExist(t *testing.T) {
	req := &nodev1.ForkSameNode{
		SourceSpawnId:    "sp-source",
		ForkSpawnId:      "sp-fork",
		SourceGeneration: 9,
		TargetGeneration: 1,
		TransferSetId:    "ts-1",
	}
	msg := &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: req}}
	if msg.GetForkSameNode().GetSourceSpawnId() != "sp-source" {
		t.Fatalf("fork request not threaded: %+v", msg)
	}
	complete := &nodev1.ForkSameNodeComplete{
		SourceSpawnId:   "sp-source",
		ForkSpawnId:     "sp-fork",
		TransferSetId:   "ts-1",
		Mounts:          []*nodev1.MountMarker{{Name: "work", Marker: "kopia-manifest"}},
		RootfsArtifacts: []*nodev1.RootfsArtifact{{ArtifactId: "rootfs-1", Generation: 1, Sequence: 1}},
	}
	reply := &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ForkSameNodeComplete{ForkSameNodeComplete: complete}}
	if reply.GetForkSameNodeComplete().GetForkSpawnId() != "sp-fork" {
		t.Fatalf("fork complete not threaded: %+v", reply)
	}
	gate := &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}}
	if gate.GetForkTurnBoundary().GetTransferSetId() != "ts-1" {
		t.Fatalf("fork turn-boundary not threaded: %+v", gate)
	}
	gateDone := &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ForkTurnBoundaryComplete{ForkTurnBoundaryComplete: &nodev1.ForkTurnBoundaryComplete{
		SourceSpawnId: "sp-source", TransferSetId: "ts-1",
	}}}
	if gateDone.GetForkTurnBoundaryComplete().GetSourceSpawnId() != "sp-source" {
		t.Fatalf("fork turn-boundary complete not threaded: %+v", gateDone)
	}
	unpause := &nodev1.CPMessage{Msg: &nodev1.CPMessage_UnpauseIfPaused{UnpauseIfPaused: &nodev1.UnpauseIfPaused{
		SpawnId: "sp-source", Generation: 9,
	}}}
	if unpause.GetUnpauseIfPaused().GetGeneration() != 9 {
		t.Fatalf("unpause command not threaded: %+v", unpause)
	}
	unpauseDone := &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_UnpauseIfPausedComplete{UnpauseIfPausedComplete: &nodev1.UnpauseIfPausedComplete{
		SpawnId: "sp-source", Generation: 9,
	}}}
	if unpauseDone.GetUnpauseIfPausedComplete().GetSpawnId() != "sp-source" {
		t.Fatalf("unpause complete not threaded: %+v", unpauseDone)
	}

	export := &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTransferExport{ForkTransferExport: &nodev1.ForkTransferExport{
		SourceSpawnId: "sp-source", ForkSpawnId: "sp-fork", SourceGeneration: 9, TargetGeneration: 1,
		TransferSetId: "ts-1", TargetNodeId: "node-2", TargetNodeClass: "cloud",
		TargetSignedSubkey: []byte("signed-subkey"), TargetNodeCertChain: []byte("leaf-chain"),
	}}}
	if string(export.GetForkTransferExport().GetTargetSignedSubkey()) != "signed-subkey" {
		t.Fatal("lost target subkey")
	}
	exported := &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ForkTransferExported{ForkTransferExported: &nodev1.ForkTransferExported{
		SourceSpawnId: "sp-source", ForkSpawnId: "sp-fork", TransferSetId: "ts-1",
		SealedTransferKey: []byte("sealed-key"), Payload: []byte("sealed-payload"),
	}}}
	if string(exported.GetForkTransferExported().GetSealedTransferKey()) != "sealed-key" {
		t.Fatal("lost sealed transfer key")
	}
	importReq := &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTransferImport{ForkTransferImport: &nodev1.ForkTransferImport{
		SourceSpawnId: "sp-source", ForkSpawnId: "sp-fork", TargetGeneration: 1, TransferSetId: "ts-1",
		SealedTransferKey: []byte("sealed-key"), Payload: []byte("sealed-payload"),
	}}}
	if string(importReq.GetForkTransferImport().GetPayload()) != "sealed-payload" {
		t.Fatal("lost import payload")
	}
}

func TestCPContractSurface(t *testing.T) {
	// per-mount backend choices now ride the CreateSpawn request
	_ = &cpv1.CreateSpawnRequest{AppId: "a", Model: "m",
		Mounts: []*cpv1.MountBinding{{Name: "main", BackendUri: "managed:repo"}}}
	// new lifecycle RPC request/response messages exist
	_ = &cpv1.ListSpawnsRequest{}
	_ = &cpv1.ListSpawnsResponse{Spawns: []*cpv1.SpawnSummary{
		{SpawnId: "sp1", Status: cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED}}}
	_ = &cpv1.SuspendSpawnRequest{SpawnId: "sp1"}
	_ = &cpv1.ResumeSpawnRequest{SpawnId: "sp1"}
	_ = &cpv1.RecreateSpawnRequest{SpawnId: "sp1"}
	_ = &cpv1.DeleteSpawnRequest{SpawnId: "sp1", DestroyData: true}
}

func TestAgentSelectionContract(t *testing.T) {
	// CreateSpawn carries the agent selection
	_ = &cpv1.CreateSpawnRequest{AppId: "a", Model: "m", Image: "ghcr.io/acme/goose:1", RunnableId: "goose-acp"}
	// ListAgentImages request/response surface
	_ = &cpv1.ListAgentImagesRequest{}
	_ = &cpv1.ListAgentImagesResponse{Images: []*cpv1.AgentImageInfo{
		{Image: "ghcr.io/acme/goose:1", CreatedAt: 5, Binaries: []string{"goose"}}}}
	// Register advertises the image's binaries
	_ = &nodev1.Register{NodeId: "n1", AgentImages: []string{"img:1"}, Binaries: []string{"goose", "opencode"}}

	// round-trip StartSpawn proves the new image/runnable/mode fields encode on the wire
	start := &nodev1.StartSpawn{
		SpawnId: "sp1", AppRef: "ref", Model: "m", Generation: 1,
		Image: "ghcr.io/acme/goose:1", RunnableId: "goose-acp", Mode: "acp",
	}
	b, err := proto.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	var got nodev1.StartSpawn
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Image != "ghcr.io/acme/goose:1" || got.RunnableId != "goose-acp" || got.Mode != "acp" {
		t.Fatalf("StartSpawn agent selection lost on round-trip: %+v", &got)
	}
}
