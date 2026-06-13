package cp

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

// goAckStarts spins a goroutine that acks (as ACTIVE) every StartSpawn `sender` is asked to deliver,
// so a Provision onto that node completes. Returns a stop func.
func goAckStarts(s *Server, sender *capSender) func() {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		acked := map[string]bool{}
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, st := range sender.starts() {
				if !acked[st.GetSpawnId()] {
					acked[st.GetSpawnId()] = true
					s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return func() { close(stop); wg.Wait() }
}

// addNode registers a node in the registry with the given class/owner/free capacity.
func addNode(reg *registry.Registry, id, class, owner string, free uint32, sender registry.NodeSender) {
	reg.Add(&registry.Node{ID: id, Sender: sender, Max: free, Free: free, Class: class, Owner: owner})
}

// MigrateSpawn suspends the spawn on its source node and resumes it on the forced target node, at a
// bumped generation, with the route rebound to the target.
func TestMigrateSpawnSuspendsThenResumesOnTarget(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "gen1-marker"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src) // source = n1, route bound, gen 1

	tgt := &capSender{}
	addNode(reg, "n2", "cloud", "", 5, tgt) // target, multi-tenant, has capacity
	stop := goAckStarts(s, tgt)
	defer stop()

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1", TargetNodeId: "n2"}))
	if err != nil {
		t.Fatalf("MigrateSpawn: %v", err)
	}
	if resp.Msg.NodeId != "n2" {
		t.Fatalf("resumed on node %q, want n2", resp.Msg.NodeId)
	}
	if resp.Msg.TransferSetId == "" {
		t.Fatal("MigrateSpawn must return a transfer_set_id")
	}
	// The source was asked to suspend at the live generation (1).
	src.mu.Lock()
	gotSuspend, lastGen := src.gotSuspend, src.lastGen
	src.mu.Unlock()
	if !gotSuspend || lastGen != 1 {
		t.Fatalf("source suspend=%v gen=%d, want true/1", gotSuspend, lastGen)
	}
	// The spawn is active on n2 at a NEW generation (2), route rebound to n2.
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Status != store.Active {
		t.Fatalf("status=%v want active", sp.Status)
	}
	c, ok, _ := s.st.Spawns().LiveContainer(ctx, "sp1")
	if !ok || c.NodeID != "n2" || c.Generation != 2 {
		t.Fatalf("live container = %+v ok=%v, want node n2 gen 2", c, ok)
	}
	// The suspend persisted the journal marker (so the migrated spawn can restore on the target).
	mounts, _ := s.st.Spawns().GetMounts(ctx, "sp1")
	if len(mounts) != 1 || mounts[0].PersistMarker != "gen1-marker" {
		t.Fatalf("mounts=%+v, want one main mount with marker gen1-marker", mounts)
	}
	ts, err := s.st.TransferSets().Get(ctx, resp.Msg.TransferSetId)
	if err != nil {
		t.Fatalf("Get transfer set: %v", err)
	}
	if ts.SpawnID != "sp1" || ts.SourceGeneration != 1 || ts.TargetGeneration != 2 {
		t.Fatalf("transfer set generations = %+v", ts)
	}
	if ts.SourceNodeID != "n1" || ts.TargetNodeID != "n2" {
		t.Fatalf("transfer set nodes = %+v", ts)
	}
	if ts.Status != store.TransferSetActive {
		t.Fatalf("transfer set status = %s, want active", ts.Status)
	}
	if ts.TransferKeyStatus != store.TransferKeyTargetReady {
		t.Fatalf("transfer key status = %s, want target_ready", ts.TransferKeyStatus)
	}
}

// MigrateSpawn to a class target (cloud) picks an eligible cloud node.
func TestMigrateSpawnClassTargetPicksCloud(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "m"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)

	tgt := &capSender{}
	addNode(reg, "cloud-1", "cloud", "", 3, tgt)
	stop := goAckStarts(s, tgt)
	defer stop()

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1", TargetClass: "cloud"}))
	if err != nil {
		t.Fatalf("MigrateSpawn(class=cloud): %v", err)
	}
	if resp.Msg.NodeId != "cloud-1" {
		t.Fatalf("resumed on %q, want cloud-1", resp.Msg.NodeId)
	}
	ts, err := s.st.TransferSets().Get(ctx, resp.Msg.TransferSetId)
	if err != nil {
		t.Fatalf("Get transfer set: %v", err)
	}
	if ts.TargetNodeID != "cloud-1" {
		t.Fatalf("class-target transfer set target node = %q, want cloud-1", ts.TargetNodeID)
	}
}

// A foreign self-hosted target is rejected up-front (PermissionDenied) and the spawn is left UNTOUCHED
// (still active on its source) — tenancy holds even under a forced placement.
func TestMigrateSpawnForeignSelfHostedTargetRejected(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	addNode(reg, "bobs-box", "self-hosted", "bob", 5, &capSender{}) // bob's node, not alice's

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1", TargetNodeId: "bobs-box"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign self-hosted target: want PermissionDenied, got %v", err)
	}
	// The spawn was never suspended.
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Status != store.Active {
		t.Fatalf("status=%v want active (untouched after rejected migrate)", sp.Status)
	}
	src.mu.Lock()
	gotSuspend := src.gotSuspend
	src.mu.Unlock()
	if gotSuspend {
		t.Fatal("source must NOT be suspended when the target is rejected up-front")
	}
}

// An owner's OWN self-hosted node is a valid target (tenancy allows it).
func TestMigrateSpawnOwnSelfHostedTargetAllowed(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "m"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	tgt := &capSender{}
	addNode(reg, "alice-box", "self-hosted", "alice", 5, tgt)
	stop := goAckStarts(s, tgt)
	defer stop()

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1", TargetNodeId: "alice-box"}))
	if err != nil {
		t.Fatalf("MigrateSpawn to own self-hosted node: %v", err)
	}
	if resp.Msg.NodeId != "alice-box" {
		t.Fatalf("resumed on %q, want alice-box", resp.Msg.NodeId)
	}
}

// A resume-on-target failure (target has no capacity) leaves a DEFINED state: the spawn rolls BACK to
// suspended (not error, not a half-migrated hang), the suspend markers survive, and no live container
// remains.
func TestMigrateSpawnResumeFailureRevertsToSuspended(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "safe-marker"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	// Target exists and is tenancy-eligible, but has ZERO free capacity -> Provision can't place it.
	addNode(reg, "n2", "cloud", "", 0, &capSender{})

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1", TargetNodeId: "n2"}))
	if err == nil {
		t.Fatal("MigrateSpawn must fail when the target cannot be placed")
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Status != store.Suspended {
		t.Fatalf("status=%v want suspended (defined revert after failed migrate)", sp.Status)
	}
	if _, ok, _ := s.st.Spawns().LiveContainer(ctx, "sp1"); ok {
		t.Fatal("a reverted migrate must leave no live container")
	}
	// The source suspend's marker survived — the user's data is safe and resumable.
	mounts, _ := s.st.Spawns().GetMounts(ctx, "sp1")
	if len(mounts) != 1 || mounts[0].PersistMarker != "safe-marker" {
		t.Fatalf("mounts=%+v, want main mount with marker safe-marker after revert", mounts)
	}
	if rt.Bound("sp1") {
		t.Fatal("route must be dropped after a reverted migrate")
	}
}

// MigrateSpawn input + auth guards.
func TestMigrateSpawnGuards(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	ctx := auth.WithOwner(context.Background(), "alice")

	// No target.
	if _, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("no target: want InvalidArgument, got %v", err)
	}
	// Both target node + class.
	if _, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1", TargetNodeId: "n2", TargetClass: "cloud"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("both targets: want InvalidArgument, got %v", err)
	}
	// Unknown target node (not connected).
	if _, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1", TargetNodeId: "ghost"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("unknown target node: want FailedPrecondition, got %v", err)
	}
	// Foreign owner.
	bob := auth.WithOwner(context.Background(), "bob")
	if _, err := s.MigrateSpawn(bob, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1", TargetNodeId: "n2"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign owner: want PermissionDenied, got %v", err)
	}
	// Unknown spawn.
	if _, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "nope", TargetNodeId: "n2"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown spawn: want NotFound, got %v", err)
	}
	// Unauthenticated.
	if _, err := s.MigrateSpawn(context.Background(), connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1", TargetNodeId: "n2"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no owner: want Unauthenticated, got %v", err)
	}
	_ = rt
}
