package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
	"spawnery/internal/secrets/journalkey"
)

// sealKey puts a dummy journal-key ciphertext (owner-sealed envelope JSON) for the given spawn+mount.
func sealKey(t *testing.T, s *Server, spawnID, mount string) {
	t.Helper()
	// A minimal valid JSON envelope (contents are opaque to the CP — just needs to be non-empty bytes).
	dummyCiphertext := []byte(`{"at_rest":{"account_id":"a","secret_id":"s","version":1},"recipients":[],"nonce":"AAAAAAAAAAAAAAAA","ct":""}`)
	ctx := context.Background()
	if err := s.journalKeys.Put(ctx, spawnID, journalkey.SecretID(mount), dummyCiphertext); err != nil {
		t.Fatalf("sealKey: Put: %v", err)
	}
}

// TestDurabilityGuardNodeLocalCrossNodeRejected: a node-local mount on a cross-node move must
// fail with FailedPrecondition and leave the spawn untouched (still active, not suspended).
func TestDurabilityGuardNodeLocalCrossNodeRejected(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "m"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	// Seed an app version declaring durability="node-local" for "main".
	seedNodeLocalAppVersion(t, s, activeSpawnTestAppID)
	addNode(reg, "n2", "cloud", "", 5, &capSender{})

	// No journal-key ciphertext + node-local manifest → node-local classification, no upgrade flag.
	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{
		SpawnId:      "sp1",
		TargetNodeId: "n2",
		// upgrade_to_owner_sealed = false (default)
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("node-local cross-node without upgrade flag: want FailedPrecondition, got %v", err)
	}
	// Spawn must still be active (untouched).
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Status != store.Active {
		t.Fatalf("spawn status=%v, want active (untouched after durability reject)", sp.Status)
	}
	// Source must NOT have been asked to suspend.
	src.mu.Lock()
	gotSuspend := src.gotSuspend
	src.mu.Unlock()
	if gotSuspend {
		t.Fatal("source must NOT be suspended when durability guard rejects the move")
	}
}

// TestDurabilityGuardUpgradeFlagWithNoCiphertextRejected: the upgrade flag set but ciphertext still
// missing → still FailedPrecondition (fail-closed).
func TestDurabilityGuardUpgradeFlagWithNoCiphertextRejected(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "m"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	seedNodeLocalAppVersion(t, s, activeSpawnTestAppID)
	addNode(reg, "n2", "cloud", "", 5, &capSender{})

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{
		SpawnId:              "sp1",
		TargetNodeId:         "n2",
		UpgradeToOwnerSealed: true, // flag set but no ciphertext
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("upgrade flag but no ciphertext: want FailedPrecondition, got %v", err)
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Status != store.Active {
		t.Fatalf("spawn must be active after failed durability check, got %v", sp.Status)
	}
}

// TestDurabilityGuardOwnerSealedCrossNodeAllowed: mounts that are owner-sealed may move cross-node.
func TestDurabilityGuardOwnerSealedCrossNodeAllowed(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "m"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	tgt := &capSender{}
	addNode(reg, "n2", "cloud", "", 5, tgt)
	stop := goAckStarts(s, tgt)
	defer stop()

	// Pre-store the owner-sealed ciphertext (simulates a completed upgrade or initial owner-sealed setup).
	sealKey(t, s, "sp1", "main")

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{
		SpawnId:      "sp1",
		TargetNodeId: "n2",
	}))
	if err != nil {
		t.Fatalf("owner-sealed cross-node move: %v", err)
	}
	if resp.Msg.NodeId != "n2" {
		t.Fatalf("expected n2, got %q", resp.Msg.NodeId)
	}
}

// TestDurabilityGuardEphemeralCrossNodeAllowed: ephemeral mounts (no journal-key ciphertext AND
// no durability declaration → default ephemeral) may move cross-node (data doesn't travel).
func TestDurabilityGuardEphemeralCrossNodeAllowed(t *testing.T) {
	s, reg, rt := newTestServer(t)
	// No mount marker needed for ephemeral (no journal).
	src := &suspendSender{}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	tgt := &capSender{}
	addNode(reg, "n2", "cloud", "", 5, tgt)
	stop := goAckStarts(s, tgt)
	defer stop()

	// No ciphertext, no manifest → ephemeral (default). Move is allowed.
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{
		SpawnId:      "sp1",
		TargetNodeId: "n2",
	}))
	if err != nil {
		t.Fatalf("ephemeral cross-node move: %v", err)
	}
	if resp.Msg.NodeId != "n2" {
		t.Fatalf("expected n2, got %q", resp.Msg.NodeId)
	}
}

// TestDurabilityGuardSameNodeNodeLocalAllowed: same-node moves bypass the durability guard.
// We use a capSender that also has the suspend behaviour baked in via a combinedSender wrapper.
// The key invariant is: no ciphertext, same-node, no flag → must succeed (guard skipped).
func TestDurabilityGuardSameNodeNodeLocalAllowed(t *testing.T) {
	s, reg, rt := newTestServer(t)
	// Use a suspendSender for the source (needed for suspend ACK).
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "m"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	seedNodeLocalAppVersion(t, s, activeSpawnTestAppID)

	// Find the node that was used.
	c, ok, _ := s.st.Spawns().LiveContainer(context.Background(), "sp1")
	if !ok {
		t.Fatal("no live container")
	}
	sameNode := c.NodeID

	// For the migrate's resume leg we also need a capSender on the same node
	// that will ack the StartSpawn. Swap it in (the source node now handles both suspend and start).
	tgtCap := &capSender{}
	// Re-add the same node with the capSender so the resume placement picks it.
	addNode(reg, sameNode, "cloud", "", 5, tgtCap)
	stop := goAckStarts(s, tgtCap)
	defer stop()
	_ = rt

	// The same-node move guard should not fire even without a ciphertext.
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{
		SpawnId:      "sp1",
		TargetNodeId: sameNode,
	}))
	if err != nil {
		t.Fatalf("same-node node-local move: %v", err)
	}
	_ = resp
}

func TestResumeSpawnPinsJournaledMountToPreviousNode(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "m"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	sealKey(t, s, "sp1", "main")

	other := &capSender{}
	addNode(reg, "n2", "cloud", "", 5, other)
	stopOther := goAckStarts(s, other)
	defer stopOther()

	ctx := auth.WithOwner(context.Background(), "alice")
	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"})); err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}

	previous := &capSender{}
	addNode(reg, "n1", "cloud", "", 1, previous)
	stopPrevious := goAckStarts(s, previous)
	defer stopPrevious()

	if _, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: "sp1"})); err != nil {
		t.Fatalf("ResumeSpawn: %v", err)
	}
	c, ok, err := s.st.Spawns().LiveContainer(ctx, "sp1")
	if err != nil || !ok {
		t.Fatalf("LiveContainer after resume: ok=%v err=%v", ok, err)
	}
	if c.NodeID != "n1" {
		t.Fatalf("journaled plain resume node=%q, want previous node n1", c.NodeID)
	}
	if len(other.starts()) != 0 {
		t.Fatalf("journaled plain resume sent StartSpawn to n2: %+v", other.starts())
	}
}

// TestDurabilityClassifyOwnerSealed: classifyMounts returns owner-sealed when ciphertext exists.
func TestDurabilityClassifyOwnerSealed(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	sealKey(t, s, "sp1", "main")

	classes, err := s.classifyMounts(context.Background(), "sp1")
	if err != nil {
		t.Fatalf("classifyMounts: %v", err)
	}
	if classes["main"] != mountClassOwnerSealed {
		t.Fatalf("main should be owner-sealed, got %v", classes["main"])
	}
	_ = reg
	_ = rt
}

func TestSeedPersistsManifestDurabilityForClassification(t *testing.T) {
	s, reg, rt := newTestServer(t)
	seedForkSource(t, s, reg, rt, "sp1", "alice", "n1", &capSender{})

	classes, err := s.classifyMounts(context.Background(), "sp1")
	if err != nil {
		t.Fatalf("classifyMounts: %v", err)
	}
	if classes["main"] != mountClassNodeLocal {
		t.Fatalf("Seeded secret-app main class=%v, want node-local from spawneryapp.yml", classes["main"])
	}
}

// TestDurabilityClassifyEphemeral: classifyMounts defaults to ephemeral when no ciphertext and no
// manifest durability declaration.
func TestDurabilityClassifyEphemeral(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)

	classes, err := s.classifyMounts(context.Background(), "sp1")
	if err != nil {
		t.Fatalf("classifyMounts: %v", err)
	}
	// No ciphertext, no manifest → ephemeral.
	if classes["main"] != mountClassEphemeral {
		t.Fatalf("main should be ephemeral (no ciphertext, no manifest), got %v", classes["main"])
	}
	_ = reg
	_ = rt
}

// TestDurabilityClassifyNodeLocal: classifyMounts returns node-local when manifest declares it.
func TestDurabilityClassifyNodeLocal(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	seedNodeLocalAppVersion(t, s, activeSpawnTestAppID)

	classes, err := s.classifyMounts(context.Background(), "sp1")
	if err != nil {
		t.Fatalf("classifyMounts: %v", err)
	}
	if classes["main"] != mountClassNodeLocal {
		t.Fatalf("main should be node-local (manifest declares it), got %v", classes["main"])
	}
	_ = reg
	_ = rt
}

// TestDurabilityGuardUpgradeFlagWithCiphertextProceeds: upgrade flag + ciphertext present → move proceeds.
func TestDurabilityGuardUpgradeFlagWithCiphertextProceeds(t *testing.T) {
	s, reg, rt := newTestServer(t)
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "m"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)
	tgt := &capSender{}
	addNode(reg, "n2", "cloud", "", 5, tgt)
	stop := goAckStarts(s, tgt)
	defer stop()

	// Simulate the UpgradeToOwnerSealed having completed (ciphertext now stored).
	sealKey(t, s, "sp1", "main")

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{
		SpawnId:              "sp1",
		TargetNodeId:         "n2",
		UpgradeToOwnerSealed: true,
	}))
	if err != nil {
		t.Fatalf("upgrade + ciphertext present: %v", err)
	}
	if resp.Msg.NodeId != "n2" {
		t.Fatalf("expected n2, got %q", resp.Msg.NodeId)
	}
}

// seedNodeLocalAppVersion registers a minimal app version with durability="node-local" on the
// "main" mount. This lets classifyMounts detect node-local class even before any ciphertext exists.
func seedNodeLocalAppVersion(t *testing.T, s *Server, appID string) {
	t.Helper()
	ctx := context.Background()
	// Must upsert the app first (FK constraint).
	if err := s.st.Apps().Upsert(ctx, store.App{
		ID: appID, DisplayName: "Node Local App", Summary: "test",
		Tags: "", Visibility: "public", Listed: false, CreatorID: "test", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("seedNodeLocalAppVersion: upsert app: %v", err)
	}
	// Protojson: the durability field is stored as "durability" in the JSON blob.
	manifestJSON := `{"mounts":[{"name":"main","durability":"node-local"}]}`
	if err := s.st.Apps().UpsertVersion(ctx, store.AppVersion{
		AppID: appID, Version: "1.0.0", Ref: "test-ref",
		Tier:     store.TierReviewed,
		Manifest: manifestJSON, CreatedAt: 1,
	}, nil); err != nil {
		t.Fatalf("seedNodeLocalAppVersion: upsert version: %v", err)
	}
}
