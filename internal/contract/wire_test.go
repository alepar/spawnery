package contract

import (
	"testing"

	"google.golang.org/protobuf/proto"

	nodev1 "spawnery/gen/node/v1"
)

func TestNodeContractFields(t *testing.T) {
	// generation threaded onto every CP->node command + onto SpawnStatus
	start := &nodev1.StartSpawn{
		SpawnId: "sp1", AppRef: "ref", DataRef: "", Model: "m",
		Generation: 7,
		Mounts:     []*nodev1.MountBinding{{Name: "main", BackendUri: "managed:repo"}},
	}
	_ = &nodev1.StopSpawn{SpawnId: "sp1", Generation: 7}
	_ = &nodev1.Suspend{SpawnId: "sp1", Generation: 7}
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
	if got.Generation != 7 || len(got.Mounts) != 1 || got.Mounts[0].Name != "main" {
		t.Fatalf("round-trip lost fields: %+v", &got)
	}

	// round-trip a nested-repeated field (SuspendComplete.Markers) — most likely to mis-serialize
	sc := &nodev1.SuspendComplete{SpawnId: "sp1", Generation: 7,
		Markers: []*nodev1.MountMarker{{Name: "main", Marker: "spawnery-suspend/sp1/7"}}}
	scb, err := proto.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	var gotSC nodev1.SuspendComplete
	if err := proto.Unmarshal(scb, &gotSC); err != nil {
		t.Fatal(err)
	}
	if gotSC.Generation != 7 || len(gotSC.Markers) != 1 || gotSC.Markers[0].Marker != "spawnery-suspend/sp1/7" {
		t.Fatalf("SuspendComplete round-trip lost fields: %+v", &gotSC)
	}
}
