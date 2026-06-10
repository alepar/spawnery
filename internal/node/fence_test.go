package node

import (
	"context"
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

// A Stop carrying a generation older than the spawn the node currently runs is stale (it targets a
// container already superseded by a Recreate) and must be ignored — otherwise it would tear down the
// fresh container. A Stop with the matching generation tears down as usual. Covers sp-8hf item 2.
func TestStopFencesStaleGeneration(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	a := newAttacher(newGooseManager(t, be), &fakeCPStream{})
	ctx := context.Background()

	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m", Generation: 5})
	if a.pumps[zeroKey("sp1")] == nil {
		t.Fatal("spawn not started")
	}

	// Stale Stop (gen 4 < live 5): ignored.
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Stop{Stop: &nodev1.StopSpawn{SpawnId: "sp1", Generation: 4}}})
	if be.wasStopped() {
		t.Fatal("stale-generation Stop must not tear down the live spawn")
	}
	a.mu.Lock()
	survived := a.pumps[zeroKey("sp1")] != nil
	a.mu.Unlock()
	if !survived {
		t.Fatal("pump must survive a stale-generation Stop")
	}

	// Current Stop (gen 5 == live 5): tears down.
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Stop{Stop: &nodev1.StopSpawn{SpawnId: "sp1", Generation: 5}}})
	if !be.wasStopped() {
		t.Fatal("matching-generation Stop must tear down the spawn")
	}
}
