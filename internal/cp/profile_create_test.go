package cp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

// seedProfile creates an owner+profile and (optionally) one custom MCP entry, returning the
// profile id and its current version. Uses the store directly (no RPC/auth ctx needed).
func seedProfile(t *testing.T, s *Server, profileID, ownerID string, withEntry bool) uint64 {
	t.Helper()
	ctx := context.Background()
	if err := s.st.Owners().Upsert(ctx, store.Owner{ID: ownerID, CreatedAt: 1}); err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	if err := s.st.Profiles().Create(ctx, store.Profile{
		ProfileID: profileID, OwnerID: ownerID, Name: "p", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	ver := uint64(1)
	if withEntry {
		// Use a valid stdio MCP payload (spec.MCPPayload with Stdio set).
		mcp, _ := json.Marshal(map[string]any{
			"stdio": map[string]any{"command": "echo", "args": []string{"hi"}},
		})
		v, err := s.st.Profiles().AddEntry(ctx, profileID, ver, store.ProfileEntry{
			EntryID: "e1", Kind: store.ProfileEntryMCP, Name: "m1",
			SourceKind: store.ProfileSourceCustom, CustomInline: mcp,
		}, 2000)
		if err != nil {
			t.Fatalf("add entry: %v", err)
		}
		ver = v
	}
	return ver
}

// ackActive drives the spawn to ACTIVE on first StartSpawn (mirrors the goroutine pattern in
// TestCreateSpawnPersistsArtifacts).
func ackActive(s *Server, sender *capSender) {
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

func TestCreateSpawnWithProfileSnapshotAndArtifacts(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	ver := seedProfile(t, s, "pf1", "alice", true)
	ackActive(s, sender)
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", ProfileId: "pf1",
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	sp, err := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sp.ProfileID != "pf1" || sp.ProfileVersion != ver {
		t.Fatalf("snapshot = %q/%d, want pf1/%d", sp.ProfileID, sp.ProfileVersion, ver)
	}
	arts, err := s.st.Spawns().GetArtifacts(ctx, resp.Msg.SpawnId)
	if err != nil {
		t.Fatalf("GetArtifacts: %v", err)
	}
	// Assembly emits exactly the manifest.json BYTES artifact for an MCP-only profile (no TAR payload).
	if len(arts) != 1 || arts[0].ArtifactID != "manifest" || arts[0].DestPath != "manifest.json" {
		t.Fatalf("artifacts = %+v", arts)
	}
}

func TestCreateSpawnSecretsOnlyProfileNoManifest(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	ver := seedProfile(t, s, "pf2", "alice", false) // empty profile -> assembly returns nil,nil
	ackActive(s, sender)
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", ProfileId: "pf2",
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	sp, _ := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
	if sp.ProfileID != "pf2" || sp.ProfileVersion != ver {
		t.Fatalf("snapshot = %q/%d", sp.ProfileID, sp.ProfileVersion)
	}
	arts, _ := s.st.Spawns().GetArtifacts(ctx, resp.Msg.SpawnId)
	if len(arts) != 0 {
		t.Fatalf("expected no artifacts for empty profile, got %+v", arts)
	}
}

func TestCreateSpawnUnknownProfileNotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", ProfileId: "nope",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestCreateSpawnForeignProfileNotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedProfile(t, s, "pf3", "bob", true) // owned by bob
	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", ProfileId: "pf3",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("want NotFound (no enumeration), got %v", err)
	}
}

func TestCreateSpawnProfileArtifactsRelayedOnStartSpawn(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	seedProfile(t, s, "pf4", "alice", true)
	ackActive(s, sender)
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", ProfileId: "pf4",
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	waitActive(t, s, resp.Msg.SpawnId)
	st := sender.firstStart()
	if st == nil || len(st.Artifacts) != 1 || st.Artifacts[0].Id != "manifest" {
		t.Fatalf("StartSpawn.Artifacts = %+v", st.GetArtifacts())
	}
}
