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

func seedVersions(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	// CreatedAt must exceed the seed's 1.0.0 (created at time.Now()) so that
	// 2.0.0 is the newest reviewed version that LatestReviewed selects.
	if err := s.st.Apps().UpsertVersion(ctx,
		store.AppVersion{AppID: "secret-app", Version: "2.0.0", Ref: "examples/secret-app", Tier: store.TierReviewed, CreatedAt: 1<<40 + 100}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.st.Apps().UpsertVersion(ctx,
		store.AppVersion{AppID: "secret-app", Version: "3.0.0-rc1", Ref: "examples/secret-app", Tier: store.TierUnverified, CreatedAt: 1<<40 + 200}, nil); err != nil {
		t.Fatal(err)
	}
}

func createActive(t *testing.T, s *Server, reg *registry.Registry, req *cpv1.CreateSpawnRequest) store.Spawn {
	t.Helper()
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	sp, err := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return sp
}

func TestCreateSpawnExplicitVersionAndPin(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedVersions(t, s)
	sp := createActive(t, s, reg, &cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m", Version: "2.0.0", Pin: true})
	if sp.AppVersion != "2.0.0" || sp.AppRef != "examples/secret-app" || !sp.Pinned {
		t.Fatalf("explicit+pin spawn = %+v (want 2.0.0/examples/secret-app, pinned)", sp)
	}
}

func TestCreateSpawnLatestVersionNotPinned(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedVersions(t, s)
	sp := createActive(t, s, reg, &cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"})
	if sp.AppVersion != "2.0.0" || sp.Pinned {
		t.Fatalf("latest spawn = %+v (want 2.0.0 newest reviewed, not pinned)", sp)
	}
}

func TestCreateSpawnVersionErrors(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedVersions(t, s)
	ctx := auth.WithOwner(context.Background(), "alice")
	if _, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m", Version: "9.9.9"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown version: want InvalidArgument, got %v", err)
	}
	if _, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m", Version: "3.0.0-rc1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("unverified version from non-creator: want PermissionDenied, got %v", err)
	}
}
