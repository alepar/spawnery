package cp

import (
	"context"
	"errors"
	"strings"
	"testing"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

// --- sp-mwco.4.5: resume gating was spike-answered KILLED (never implemented). These tests guard
// the invariant that follows from that: every start (create AND resume alike) re-stats and
// re-presigns by-ref artifacts through nodeArtifactsForStart -- there is no gated/cached path to
// bypass on resume. See docs/superpowers/specs/2026-07-12-skill-delivery-hardening-design.md §4.5. ---

// TestNodeArtifactsForStart_StatsAndPresignsOnEveryCall asserts that calling
// nodeArtifactsForStart a second time with the same by-ref artifact set (the CP-side shape of a
// resume re-thread -- ResumeSpawn/RecreateSpawn/ForkSpawn all funnel through this one call site,
// per its doc comment) still returns a fresh, non-empty PresignedUrl for every by-ref spec. No
// gating means no reuse of a stale URL and no skipped presign call on the second pass.
func TestNodeArtifactsForStart_StatsAndPresignsOnEveryCall(t *testing.T) {
	s, _, _ := newTestServer(t)
	sha := strings.Repeat("a", 64)
	fake := skillstore.NewFakeSkillStore()
	if err := fake.PutIfAbsent(context.Background(), sha, []byte("compressed"), nil); err != nil {
		t.Fatalf("seed fake store: %v", err)
	}
	s.skillStore = fake

	arts := []store.Artifact{
		{ArtifactID: "br1", ContentType: 2, TargetContainer: 1, DestPath: "payloads/e1",
			ObjectKey: "skills/" + sha + ".tar.zst", ObjectSHA256: sha},
	}

	// First call: the create path.
	out1, err := s.nodeArtifactsForStart(context.Background(), arts)
	if err != nil {
		t.Fatalf("first (create) nodeArtifactsForStart: %v", err)
	}
	if len(out1) != 1 || out1[0].Objectref == nil || out1[0].Objectref.PresignedUrl == "" {
		t.Fatalf("first call: want a presigned by-ref spec, got %+v", out1)
	}

	// Second call, same artifact set: the resume re-thread. A gated implementation could skip the
	// presign here (e.g. because statSkillObjects memoized the sha as present); assert it did not.
	out2, err := s.nodeArtifactsForStart(context.Background(), arts)
	if err != nil {
		t.Fatalf("second (resume) nodeArtifactsForStart: %v", err)
	}
	if len(out2) != 1 || out2[0].Objectref == nil || out2[0].Objectref.PresignedUrl == "" {
		t.Fatalf("second (resume) call: want a presigned by-ref spec, got %+v", out2)
	}

	var presignCalls int
	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "presign:") {
			presignCalls++
		}
	}
	if presignCalls != 2 {
		t.Fatalf("presign calls = %d, want 2 (one per nodeArtifactsForStart call, including resume)", presignCalls)
	}
}

// TestStatSkillObjects_MemoizedShaSurvivesTransportFailure documents the CP-side residual Garage
// dependency named in the §4.5 spike answer: statSkillObjects memoizes a confirmed-present sha
// (s.skillPresent), so a later call for the SAME sha tolerates a Garage transport failure --
// this is the "warm-CP-only" mitigation, not resume gating. A cold CP (nothing memoized yet)
// still needs a live Garage; that case is unchanged and already covered by the §4.4 tests.
func TestStatSkillObjects_MemoizedShaSurvivesTransportFailure(t *testing.T) {
	s, _, _ := newTestServer(t)
	sha := strings.Repeat("b", 64)
	fake := skillstore.NewFakeSkillStore()
	if err := fake.PutIfAbsent(context.Background(), sha, []byte("compressed"), nil); err != nil {
		t.Fatalf("seed fake store: %v", err)
	}
	s.skillStore = fake

	specs := []*nodev1.ArtifactSpec{
		{Id: "br1", Objectref: &nodev1.ObjectRef{ObjectKey: "skills/" + sha + ".tar.zst", Sha256: sha}},
	}

	// First stat: healthy fake, memoizes sha as present.
	if err := s.statSkillObjects(context.Background(), specs); err != nil {
		t.Fatalf("first (create) statSkillObjects: %v", err)
	}

	// Flip the fake to a transport error (simulating Garage down), then stat the SAME sha again
	// on the resume re-thread.
	fake.StatHook = func(_ context.Context, _ string) error {
		return errors.New("dial tcp: connection refused (Garage down)")
	}
	if err := s.statSkillObjects(context.Background(), specs); err != nil {
		t.Fatalf("second (resume) statSkillObjects with Garage down: got %v, want nil (memoized sha must be skipped)", err)
	}
}
