package cp

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

func TestDenySkillObject_NonAdmin_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)
	sha := strings.Repeat("1", 64)

	_, err := s.DenySkillObject(aliceCtx(), connect.NewRequest(&cpv1.DenySkillObjectRequest{Sha256: sha, Reason: "bad"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("DenySkillObject non-admin: got %v, want PermissionDenied", err)
	}

	_, err = s.AllowSkillObject(aliceCtx(), connect.NewRequest(&cpv1.AllowSkillObjectRequest{Sha256: sha}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("AllowSkillObject non-admin: got %v, want PermissionDenied", err)
	}

	_, err = s.ListSkillObjectDenials(aliceCtx(), connect.NewRequest(&cpv1.ListSkillObjectDenialsRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("ListSkillObjectDenials non-admin: got %v, want PermissionDenied", err)
	}
}

func TestDenySkillObject_BlankReason_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})
	sha := strings.Repeat("2", 64)

	_, err := s.DenySkillObject(adminCtx(), connect.NewRequest(&cpv1.DenySkillObjectRequest{Sha256: sha, Reason: "   "}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("DenySkillObject blank reason: got %v, want InvalidArgument", err)
	}
}

func TestDenySkillObject_MalformedSha_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})

	for _, bad := range []string{"", "not-hex", strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		_, err := s.DenySkillObject(adminCtx(), connect.NewRequest(&cpv1.DenySkillObjectRequest{Sha256: bad, Reason: "bad"}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("DenySkillObject(%q): got %v, want InvalidArgument", bad, err)
		}
	}
}

func TestDenySkillObject_UppercaseSha_NormalizedAndAccepted(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})
	lower := strings.Repeat("3", 64)
	upper := strings.ToUpper(lower)

	if _, err := s.DenySkillObject(adminCtx(), connect.NewRequest(&cpv1.DenySkillObjectRequest{Sha256: upper, Reason: "bad"})); err != nil {
		t.Fatalf("DenySkillObject with uppercase sha: %v", err)
	}

	got, err := s.st.SkillDenylist().Denied(context.Background(), []string{lower})
	if err != nil {
		t.Fatalf("Denied: %v", err)
	}
	if _, ok := got[lower]; !ok {
		t.Fatalf("expected the normalized lowercase sha %q to be denied, got %v", lower, got)
	}
}

// TestDenySkillObject_EndToEndRevokeUnrevokeLoop is the bead's acceptance criterion, exercised
// through the real RPC surface + real store: Deny -> a by-ref start for the sha fails
// FailedPrecondition -> Allow -> the same start succeeds again.
func TestDenySkillObject_EndToEndRevokeUnrevokeLoop(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})
	sha := strings.Repeat("4", 64)
	fake := skillstore.NewFakeSkillStore()
	if err := fake.PutIfAbsent(context.Background(), sha, []byte("compressed"), nil); err != nil {
		t.Fatalf("seed fake store: %v", err)
	}
	s.skillStore = fake
	art := store.Artifact{
		ArtifactID:      "br1",
		ContentType:     2,
		TargetContainer: 1,
		DestPath:        "payloads/e1",
		ObjectKey:       "skills/" + sha + ".tar.zst",
		ObjectSHA256:    sha,
	}

	// Before any deny: start succeeds.
	if _, err := s.nodeArtifactsForStart(context.Background(), []store.Artifact{art}); err != nil {
		t.Fatalf("nodeArtifactsForStart before deny: %v", err)
	}

	// Deny via the RPC.
	if _, err := s.DenySkillObject(adminCtx(), connect.NewRequest(&cpv1.DenySkillObjectRequest{Sha256: sha, Reason: "supply-chain compromise"})); err != nil {
		t.Fatalf("DenySkillObject: %v", err)
	}
	_, err := s.nodeArtifactsForStart(context.Background(), []store.Artifact{art})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("nodeArtifactsForStart after deny: got %v, want FailedPrecondition", err)
	}

	// Allow via the RPC undoes it.
	if _, err := s.AllowSkillObject(adminCtx(), connect.NewRequest(&cpv1.AllowSkillObjectRequest{Sha256: sha})); err != nil {
		t.Fatalf("AllowSkillObject: %v", err)
	}
	if _, err := s.nodeArtifactsForStart(context.Background(), []store.Artifact{art}); err != nil {
		t.Fatalf("nodeArtifactsForStart after allow: %v", err)
	}
}

func TestAllowSkillObject_UnknownSha_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})
	sha := strings.Repeat("5", 64)

	_, err := s.AllowSkillObject(adminCtx(), connect.NewRequest(&cpv1.AllowSkillObjectRequest{Sha256: sha}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("AllowSkillObject unknown sha: got %v, want NotFound", err)
	}
}

func TestListSkillObjectDenials_ShowsReasonAndDeniedBy(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetAdminOwners([]string{"admin"})
	sha := strings.Repeat("6", 64)

	if _, err := s.DenySkillObject(adminCtx(), connect.NewRequest(&cpv1.DenySkillObjectRequest{Sha256: sha, Reason: "leaked credential"})); err != nil {
		t.Fatalf("DenySkillObject: %v", err)
	}

	resp, err := s.ListSkillObjectDenials(adminCtx(), connect.NewRequest(&cpv1.ListSkillObjectDenialsRequest{}))
	if err != nil {
		t.Fatalf("ListSkillObjectDenials: %v", err)
	}
	if len(resp.Msg.Denials) != 1 {
		t.Fatalf("Denials = %v, want 1 entry", resp.Msg.Denials)
	}
	d := resp.Msg.Denials[0]
	if d.Sha256 != sha || d.Reason != "leaked credential" || d.DeniedBy != "admin" {
		t.Fatalf("Denials[0] = %+v, want sha256=%s reason=leaked credential denied_by=admin", d, sha)
	}
}
