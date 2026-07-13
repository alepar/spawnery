package cp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

// --- sp-mwco.3.2 §4.2: real revocation. A "delete" removes only the customization_catalog row;
// the Garage object is never removed, and an already-bound spawn replays from its own persisted
// spawn_artifacts row (object_key + sha) at resume/fork WITHOUT re-consulting the catalog. These
// tests assert the sha denylist, consulted inside presignNodeArtifacts (the single choke point
// for every start path), actually closes that gap. ---

// denylistArtifact builds a single by-ref store.Artifact whose object is present in fake, so the
// statSkillObjects HEAD gate passes and only the denylist can refuse the start.
func denylistArtifact(t *testing.T, fake *skillstore.FakeSkillStore, sha string) store.Artifact {
	t.Helper()
	if err := fake.PutIfAbsent(context.Background(), sha, []byte("compressed"), nil); err != nil {
		t.Fatalf("seed fake store: %v", err)
	}
	return store.Artifact{
		ArtifactID:      "br1",
		ContentType:     2,
		TargetContainer: 1,
		DestPath:        "payloads/e1",
		ObjectKey:       "skills/" + sha + ".tar.zst",
		ObjectSHA256:    sha,
	}
}

func TestNodeArtifactsForStart_DeniedShaBlocksPresign_Create(t *testing.T) {
	s, _, _ := newTestServer(t)
	sha := strings.Repeat("a", 64)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	art := denylistArtifact(t, fake, sha)

	if err := s.st.SkillDenylist().Deny(context.Background(), store.SkillObjectDenial{
		SHA256: sha, Reason: "malicious payload", DeniedBy: "admin", CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	_, err := s.nodeArtifactsForStart(context.Background(), []store.Artifact{art})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("nodeArtifactsForStart with denied sha: got %v, want CodeFailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), art.ObjectKey) {
		t.Errorf("error %q does not contain object key %q", err.Error(), art.ObjectKey)
	}
	if !strings.Contains(err.Error(), "malicious payload") {
		t.Errorf("error %q does not contain the denial reason", err.Error())
	}

	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "presign:") {
			t.Fatalf("expected zero presign calls for a denied sha, got %v", fake.Calls())
		}
	}
}

func TestNodeArtifactsForStart_DeniedAfterFirstStart_BlocksResumeAndFork(t *testing.T) {
	s, _, _ := newTestServer(t)
	sha := strings.Repeat("b", 64)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	art := denylistArtifact(t, fake, sha)

	// First call: the create path — succeeds and presigns (the "already-bound spawn" state).
	out, err := s.nodeArtifactsForStart(context.Background(), []store.Artifact{art})
	if err != nil {
		t.Fatalf("first (create) nodeArtifactsForStart: %v", err)
	}
	if len(out) != 1 || out[0].Objectref == nil || out[0].Objectref.PresignedUrl == "" {
		t.Fatalf("first call: want a presigned by-ref spec, got %+v", out)
	}

	// Now deny the sha — the kill switch.
	if err := s.st.SkillDenylist().Deny(context.Background(), store.SkillObjectDenial{
		SHA256: sha, Reason: "revoked after publish", DeniedBy: "admin", CreatedAt: 2000,
	}); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	// Second call, SAME artifact set: the resume re-thread. Garage object still exists and sha is
	// memoized present in s.skillPresent — only the denylist can refuse this.
	_, err = s.nodeArtifactsForStart(context.Background(), []store.Artifact{art})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("second (resume) call after deny: got %v, want CodeFailedPrecondition", err)
	}

	// Third call: the fork re-thread — same funnel, must also refuse.
	_, err = s.nodeArtifactsForStart(context.Background(), []store.Artifact{art})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("third (fork) call after deny: got %v, want CodeFailedPrecondition", err)
	}
}

// denylistLookupErrStore wraps a real store.Store and injects an error on SkillDenylist().Denied
// — the least invasive seam for a fail-closed test, per the bead's design notes (no
// production-only knobs).
type denylistLookupErrStore struct {
	store.Store
	err error
}

func (d *denylistLookupErrStore) SkillDenylist() store.SkillDenylistRepo {
	return &erroringDenylistRepo{err: d.err}
}

type erroringDenylistRepo struct{ err error }

func (r *erroringDenylistRepo) Deny(context.Context, store.SkillObjectDenial) error { return r.err }
func (r *erroringDenylistRepo) Allow(context.Context, string) error                 { return r.err }
func (r *erroringDenylistRepo) List(context.Context) ([]store.SkillObjectDenial, error) {
	return nil, r.err
}
func (r *erroringDenylistRepo) Denied(context.Context, []string) (map[string]store.SkillObjectDenial, error) {
	return nil, r.err
}

func TestPresignNodeArtifacts_DenylistLookupErrorFailsClosed(t *testing.T) {
	s, _, _ := newTestServer(t)
	sha := strings.Repeat("c", 64)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	art := denylistArtifact(t, fake, sha)

	s.st = &denylistLookupErrStore{Store: s.st, err: errors.New("dial tcp: connection refused")}

	_, err := s.nodeArtifactsForStart(context.Background(), []store.Artifact{art})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("nodeArtifactsForStart with denylist lookup error: got %v, want CodeUnavailable", err)
	}

	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "presign:") {
			t.Fatalf("expected zero presign calls on a denylist lookup error, got %v", fake.Calls())
		}
	}
}

func TestPresignNodeArtifacts_UndeniedShaStillPresigns(t *testing.T) {
	s, _, _ := newTestServer(t)
	sha := strings.Repeat("d", 64)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	art := denylistArtifact(t, fake, sha)

	out, err := s.nodeArtifactsForStart(context.Background(), []store.Artifact{art})
	if err != nil {
		t.Fatalf("nodeArtifactsForStart for an undenied sha: %v", err)
	}
	if len(out) != 1 || out[0].Objectref == nil || out[0].Objectref.PresignedUrl == "" {
		t.Fatalf("want a presigned by-ref spec, got %+v", out)
	}
}

// countingDenylistRepo counts calls to Denied, delegating everything else to the wrapped repo.
type countingDenylistRepo struct {
	store.SkillDenylistRepo
	deniedCalls int
}

func (r *countingDenylistRepo) Denied(ctx context.Context, shas []string) (map[string]store.SkillObjectDenial, error) {
	r.deniedCalls++
	return r.SkillDenylistRepo.Denied(ctx, shas)
}

type countingDenylistStore struct {
	store.Store
	repo *countingDenylistRepo
}

func (c *countingDenylistStore) SkillDenylist() store.SkillDenylistRepo { return c.repo }

func TestPresignNodeArtifacts_NoByRefSpecs_NoDenylistQuery(t *testing.T) {
	s, _, _ := newTestServer(t)
	counting := &countingDenylistRepo{SkillDenylistRepo: s.st.SkillDenylist()}
	s.st = &countingDenylistStore{Store: s.st, repo: counting}

	// Inline-only artifact: no Objectref at all.
	art := store.Artifact{
		ArtifactID:      "inline1",
		Inline:          []byte("hello"),
		ContentType:     1,
		TargetContainer: 1,
		DestPath:        "payloads/inline1",
	}
	out, err := s.nodeArtifactsForStart(context.Background(), []store.Artifact{art})
	if err != nil {
		t.Fatalf("nodeArtifactsForStart with no by-ref specs: %v", err)
	}
	if len(out) != 1 || out[0].Objectref != nil {
		t.Fatalf("want a single inline (no-Objectref) spec, got %+v", out)
	}
	if counting.deniedCalls != 0 {
		t.Fatalf("Denied() called %d times for a spec set with no by-ref artifacts, want 0", counting.deniedCalls)
	}
}
