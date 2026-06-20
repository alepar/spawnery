package cp

// github_link_preflight_test.go: hermetic tests for the CreateSpawn GitHub link preflight.
//
// Coverage:
//   - github:-mount app with checker returning none → CodeFailedPrecondition, spawn NOT persisted.
//   - github:-mount app with checker returning relink_required → CodeFailedPrecondition, spawn NOT persisted.
//   - github:-mount app with checker returning error → CodeUnavailable, spawn NOT persisted.
//   - github:-mount app with checker returning active → spawn persisted (CreateSpawn succeeds).
//   - non-github app (scratch mount only) → preflight skipped regardless of checker.
//   - nil checker → preflight always skipped (non-github lane / hermetic default).

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// fakeASLinkChecker implements asLinkChecker for tests.
type fakeASLinkChecker struct {
	status gitHubLinkStatus
	err    error
	called int
}

func (f *fakeASLinkChecker) CheckLinkStatus(_ context.Context, _ string) (gitHubLinkStatus, error) {
	f.called++
	return f.status, f.err
}

// seedGitHubSlotApp creates an app with a single github-slot mount in the test store.
func seedGitHubSlotApp(t *testing.T, s *Server, appID string) {
	t.Helper()
	ctx := context.Background()
	now := int64(0)
	if err := s.st.Apps().Upsert(ctx, store.App{
		ID: appID, DisplayName: appID, Visibility: "public", Listed: true, CreatorID: "spawnery", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	decls := []store.MountDecl{{AppID: appID, Version: "1.0.0", Name: "repo", Path: "repo", Required: true, Github: true}}
	if err := s.st.Apps().UpsertVersion(ctx,
		store.AppVersion{AppID: appID, Version: "1.0.0", Ref: "examples/" + appID, Tier: store.TierReviewed, CreatedAt: now},
		decls); err != nil {
		t.Fatal(err)
	}
}

// githubMountRequest is the CreateSpawnRequest for a github-slot app.
func githubMountRequest(appID string) *cpv1.CreateSpawnRequest {
	return &cpv1.CreateSpawnRequest{
		AppId: appID,
		Model: "m",
		Mounts: []*cpv1.MountBinding{{
			Name:       "repo",
			BackendUri: "github:owner/repo",
		}},
	}
}

// spawnCount returns the number of spawns owned by owner in the store.
func spawnCount(t *testing.T, s *Server, owner string) int {
	t.Helper()
	spawns, err := s.st.Spawns().ListByOwner(context.Background(), owner)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	return len(spawns)
}

// TestGitHubLinkPreflightNone: checker returns "none" → FailedPrecondition, spawn NOT persisted.
func TestGitHubLinkPreflightNone(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedGitHubSlotApp(t, s, "gh-app")
	checker := &fakeASLinkChecker{status: gitHubLinkStatusNone}
	s.linkChecker = checker

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(githubMountRequest("gh-app")))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want CodeFailedPrecondition, got %v", connect.CodeOf(err))
	}
	if n := spawnCount(t, s, "alice"); n != 0 {
		t.Fatalf("want 0 persisted spawns, got %d", n)
	}
	if checker.called != 1 {
		t.Fatalf("want checker called once, got %d", checker.called)
	}
}

// TestGitHubLinkPreflightRelinkRequired: checker returns "relink_required" → FailedPrecondition, spawn NOT persisted.
func TestGitHubLinkPreflightRelinkRequired(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedGitHubSlotApp(t, s, "gh-app")
	checker := &fakeASLinkChecker{status: gitHubLinkStatusRelinkRequired}
	s.linkChecker = checker

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(githubMountRequest("gh-app")))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want CodeFailedPrecondition, got %v", connect.CodeOf(err))
	}
	if n := spawnCount(t, s, "alice"); n != 0 {
		t.Fatalf("want 0 persisted spawns, got %d", n)
	}
}

// TestGitHubLinkPreflightCheckerError: checker returns error → CodeUnavailable, spawn NOT persisted.
func TestGitHubLinkPreflightCheckerError(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedGitHubSlotApp(t, s, "gh-app")
	checker := &fakeASLinkChecker{err: errors.New("AS unreachable")}
	s.linkChecker = checker

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(githubMountRequest("gh-app")))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("want CodeUnavailable, got %v", connect.CodeOf(err))
	}
	if n := spawnCount(t, s, "alice"); n != 0 {
		t.Fatalf("want 0 persisted spawns, got %d", n)
	}
}

// TestGitHubLinkPreflightActive: checker returns "active" → CreateSpawn proceeds and persists.
func TestGitHubLinkPreflightActive(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedGitHubSlotApp(t, s, "gh-app")
	checker := &fakeASLinkChecker{status: gitHubLinkStatusActive}
	s.linkChecker = checker

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(githubMountRequest("gh-app")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Msg.SpawnId == "" {
		t.Fatal("want non-empty spawn_id")
	}
	if n := spawnCount(t, s, "alice"); n != 1 {
		t.Fatalf("want 1 persisted spawn, got %d", n)
	}
	if checker.called != 1 {
		t.Fatalf("want checker called once, got %d", checker.called)
	}
}

// TestGitHubLinkPreflightSkippedForNonGitHubApp: non-github (scratch) mount app → checker never called.
func TestGitHubLinkPreflightSkippedForNonGitHubApp(t *testing.T) {
	s, _, _ := newTestServer(t)
	// "secret-app" from the default test seed has a plain (scratch) "main" mount.
	checker := &fakeASLinkChecker{status: gitHubLinkStatusNone} // would reject if called
	s.linkChecker = checker

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app",
		Model: "m",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if checker.called != 0 {
		t.Fatalf("want checker NOT called for non-github app, called %d times", checker.called)
	}
}

// TestGitHubLinkPreflightNilCheckerSkipped: nil checker → preflight is skipped entirely.
func TestGitHubLinkPreflightNilCheckerSkipped(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedGitHubSlotApp(t, s, "gh-app")
	// linkChecker is nil by default (not wired in newTestServer).

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(githubMountRequest("gh-app")))
	// Should proceed (no preflight) — the spawn will be in 'starting' with no node to pick from,
	// but CreateSpawn itself returns the spawn_id before provision completes.
	if err != nil {
		t.Fatalf("nil checker: want no preflight rejection, got %v", err)
	}
}
