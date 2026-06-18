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

func seedCreateSpawnMountApp(t *testing.T, s *Server, appID string, mountNames ...string) {
	t.Helper()

	ctx := context.Background()
	now := time.Now().Unix()
	if err := s.st.Apps().Upsert(ctx, store.App{
		ID: appID, DisplayName: appID, Visibility: "public", Listed: true, CreatorID: "spawnery", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	decls := make([]store.MountDecl, len(mountNames))
	for i, name := range mountNames {
		decls[i] = store.MountDecl{AppID: appID, Version: "1.0.0", Name: name, Required: true}
	}
	if err := s.st.Apps().UpsertVersion(ctx,
		store.AppVersion{AppID: appID, Version: "1.0.0", Ref: "examples/" + appID, Tier: store.TierReviewed, CreatedAt: now},
		decls); err != nil {
		t.Fatal(err)
	}
}

func TestCreateSpawnPersistsRequestedMountBindingsAndDefaultsOthersToScratch(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedCreateSpawnMountApp(t, s, "multi-mount-app", "main", "cache")

	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "multi-mount-app",
		Model: "m",
		Mounts: []*cpv1.MountBinding{{
			Name:               "main",
			BackendUri:         "github:owner/repo",
			CredentialSecretId: "gh-main",
			CreateIfMissing:    true,
			RepositoryId:       "123",
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	waitActive(t, s, resp.Msg.SpawnId)

	mounts, err := s.st.Spawns().GetMounts(ctx, resp.Msg.SpawnId)
	if err != nil {
		t.Fatalf("GetMounts: %v", err)
	}
	got := map[string]store.Mount{}
	for _, mount := range mounts {
		got[mount.Name] = mount
	}
	if got["main"].BackendURI != "github:owner/repo" || got["main"].CredentialSecretID != "gh-main" || !got["main"].CreateIfMissing || got["main"].RepositoryID != "123" {
		t.Fatalf("main mount = %+v, want github metadata; mounts=%+v", got["main"], mounts)
	}
	if got["cache"].BackendURI != "scratch" {
		t.Fatalf("cache backend = %q, want scratch; mounts=%+v", got["cache"].BackendURI, mounts)
	}
}

func TestCreateSpawnRejectsUndeclaredMountBinding(t *testing.T) {
	s, _, _ := newTestServer(t)

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app",
		Model: "m",
		Mounts: []*cpv1.MountBinding{{
			Name:       "bogus",
			BackendUri: "github:owner/repo",
		}},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CreateSpawn error code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}

func TestCreateSpawnRejectsDuplicateMountBindings(t *testing.T) {
	s, _, _ := newTestServer(t)

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app",
		Model: "m",
		Mounts: []*cpv1.MountBinding{
			{Name: "main", BackendUri: "scratch:"},
			{Name: "main", BackendUri: "github:owner/repo"},
		},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CreateSpawn error code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}

func TestCreateSpawnRejectsGithubMountWithoutCredentialSecretID(t *testing.T) {
	s, _, _ := newTestServer(t)

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app",
		Model: "m",
		Mounts: []*cpv1.MountBinding{{
			Name:       "main",
			BackendUri: "github:owner/repo",
		}},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CreateSpawn error code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}

// seedCreateSpawnGitHubSlotApp seeds an app whose named mounts include a github SLOT plus optional
// plain (scratch) mounts.
func seedCreateSpawnGitHubSlotApp(t *testing.T, s *Server, appID, slot string, plain ...string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	if err := s.st.Apps().Upsert(ctx, store.App{
		ID: appID, DisplayName: appID, Visibility: "public", Listed: true, CreatorID: "spawnery", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	decls := []store.MountDecl{{AppID: appID, Version: "1.0.0", Name: slot, Path: slot, Required: true, Github: true}}
	for _, name := range plain {
		decls = append(decls, store.MountDecl{AppID: appID, Version: "1.0.0", Name: name, Required: true})
	}
	if err := s.st.Apps().UpsertVersion(ctx,
		store.AppVersion{AppID: appID, Version: "1.0.0", Ref: "examples/" + appID, Tier: store.TierReviewed, CreatedAt: now},
		decls); err != nil {
		t.Fatal(err)
	}
}

func TestCreateSpawnGitHubSlotBindsRepoAndDefaultsOthersToScratch(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedCreateSpawnGitHubSlotApp(t, s, "gh-slot-app", "repo", "cache")

	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "gh-slot-app",
		Model: "m",
		Mounts: []*cpv1.MountBinding{{
			Name:            "repo",
			BackendUri:      "github:owner/repo",
			CreateIfMissing: true,
			RepositoryId:    "123",
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	waitActive(t, s, resp.Msg.SpawnId)

	mounts, err := s.st.Spawns().GetMounts(ctx, resp.Msg.SpawnId)
	if err != nil {
		t.Fatalf("GetMounts: %v", err)
	}
	got := map[string]store.Mount{}
	for _, m := range mounts {
		got[m.Name] = m
	}
	if got["repo"].BackendURI != "github:owner/repo" || got["repo"].CredentialSecretID != "" || !got["repo"].CreateIfMissing || got["repo"].RepositoryID != "123" {
		t.Fatalf("repo mount = %+v; want github backend, empty credential (T3 resolves)", got["repo"])
	}
	if got["cache"].BackendURI != "scratch" {
		t.Fatalf("cache backend = %q, want scratch", got["cache"].BackendURI)
	}
}

func TestCreateSpawnRejectsUnboundGitHubSlot(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedCreateSpawnGitHubSlotApp(t, s, "gh-slot-app2", "repo")
	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "gh-slot-app2", Model: "m",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CreateSpawn code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}
