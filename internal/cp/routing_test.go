package cp

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

func seedUnverified(t *testing.T, s *Server, creator, appID string) {
	t.Helper()
	ctx := auth.WithOwner(context.Background(), creator)
	if _, err := s.RegisterAppVersion(ctx, connect.NewRequest(&cpv1.RegisterAppVersionRequest{
		Manifest: &cpv1.AppManifest{ApiVersion: "spawnery/v1", Id: appID, Title: "T", Visibility: "open", Mounts: []*cpv1.ManifestMount{{Name: "main", Path: "data", Seed: "seed"}}},
		Version: "0.1.0", Ref: appID + "@sha",
	})); err != nil {
		t.Fatal(err)
	}
}

func createActiveOn(t *testing.T, s *Server, reg *registry.Registry, caller, appID, version, nodeClass, nodeOwner string) (store.Spawn, error) {
	t.Helper()
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1, Class: nodeClass, Owner: nodeOwner})
	go func() {
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	resp, err := s.CreateSpawn(auth.WithOwner(context.Background(), caller), connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: appID, Model: "m", Version: version}))
	if err != nil {
		return store.Spawn{}, err
	}
	waitActive(t, s, resp.Msg.SpawnId) // CreateSpawn is async; wait for the background provision
	sp, gerr := s.st.Spawns().Get(context.Background(), resp.Msg.SpawnId)
	if gerr != nil {
		t.Fatal(gerr)
	}
	return sp, nil
}

func TestUnverifiedSpawnsOnAuthorSelfHostedNode(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedUnverified(t, s, "alice", "alice/dev")
	sp, err := createActiveOn(t, s, reg, "alice", "alice/dev", "0.1.0", "self-hosted", "alice")
	if err != nil || sp.AppVersion != "0.1.0" {
		t.Fatalf("author should spawn unverified on own self-hosted node: sp=%+v err=%v", sp, err)
	}
}

func TestUnverifiedRejectedForNonCreator(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedUnverified(t, s, "alice", "alice/dev")
	_, err := createActiveOn(t, s, reg, "mallory", "alice/dev", "0.1.0", "self-hosted", "mallory")
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-creator unverified spawn want PermissionDenied, got %v", err)
	}
}

func TestUnverifiedRejectedWithoutSelfHostedNode(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedUnverified(t, s, "alice", "alice/dev")
	// Register only a cloud node; unverified app requires self-hosted node.
	// CreateSpawn is async: it returns the spawn in 'starting', then provision fails (ResourceExhausted
	// from the scheduler) in the background and SetError is called, leaving the spawn Errored.
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1, Class: "cloud", Owner: ""})
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "alice/dev", Model: "m", Version: "0.1.0"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		sp, _ := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
		if sp.Status == store.Errored {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unverified spawn w/o self-hosted node not errored within 3s (status=%v)", sp.Status)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
