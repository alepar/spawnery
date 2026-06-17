package node

// intent_seam_test.go: end-to-end enforcement tests at the node seam [AC1][AM12].
//
// These tests drive the production startSpawn path with an enforced IntentVerifier so we
// can assert that:
//   (a) a StartSpawn with nil Auth → SpawnPhase_ERROR with MISSING_INTENT (no container)
//   (b) a StartSpawn with a substituted image → ERROR with CORRESPONDENCE (no container)
//   (c) a valid matching envelope → SpawnPhase_ACTIVE (container created, agent handshakes)
//   (d) same forged StartSpawn under AuthModeVerifyLog → proceeds to ACTIVE (log-not-enforce)
//
// Tests (a)/(b) verify the gate is actually wired: the verifier runs BEFORE mgr.CreateWithSelection
// so a.active == 0 proves no container was created.
// Tests (c)/(d) exercise the full lifecycle using scriptedPodBackend + scriptGoose.

import (
	"context"
	"strings"
	"testing"
	"time"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/intent"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// lastErrorDetail returns the Detail field of the last ERROR status sent for spawnID.
func (f *fakeCPStream) lastErrorDetail(spawnID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if s := f.sent[i].GetStatus(); s != nil && s.SpawnId == spawnID && s.Phase == nodev1.SpawnPhase_ERROR {
			return s.Detail
		}
	}
	return ""
}

// newEnforcedAttacher builds an attacher with an enforced IntentVerifier (selfHosted=false,
// nodeOwner="", so only assertedOwner is validated). The verifier uses a fixed clock.
func newEnforcedAttacher(t *testing.T, mgr *spawnlet.Manager, fs cpStream, ks token.KeySet, mode AuthMode, fixedNow time.Time) *attacher {
	t.Helper()
	a := newAttacher(mgr, fs)
	a.verifier = NewIntentVerifier(ks, "", "", false, mode, func() time.Time { return fixedNow })
	return a
}

// ---- (a) nil Auth in enforced mode ---------------------------------------------------

// TestIntentSeam_NilAuthBlocked: a StartSpawn with no Auth in enforced mode must reach
// SpawnPhase_ERROR with MISSING_INTENT before any container is created.
func TestIntentSeam_NilAuthBlocked(t *testing.T) {
	_, ks := genASKey(t)
	fixedNow := time.Unix(1_770_000_000, 0)

	mgr := spawnlet.NewManager(runtime.NewFake(), spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	fs := &fakeCPStream{}
	a := newEnforcedAttacher(t, mgr, fs, ks, AuthModeEnforced, fixedNow)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId:       "sp-nil-auth",
		AppRef:        "/unused/app", // never reached
		Model:         "m",
		AssertedOwner: "alice",
		Auth:          nil,
	})

	phases := fs.phasesFor("sp-nil-auth")
	if len(phases) < 2 || phases[0] != nodev1.SpawnPhase_STARTING || lastPhase(phases) != nodev1.SpawnPhase_ERROR {
		t.Fatalf("phases = %v, want STARTING...ERROR", phases)
	}
	detail := fs.lastErrorDetail("sp-nil-auth")
	if !strings.Contains(detail, string(NACKMissingIntent)) {
		t.Fatalf("ERROR detail = %q, want to contain %s", detail, NACKMissingIntent)
	}
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active != 0 {
		t.Fatalf("active = %d, want 0 (no container must be created before gate)", active)
	}
}

// ---- (b) substituted image → CORRESPONDENCE -------------------------------------------

// TestIntentSeam_ImageSubstitutionBlocked: a StartSpawn whose image differs from the
// signed intent's image must reach ERROR with CORRESPONDENCE before any container is created.
func TestIntentSeam_ImageSubstitutionBlocked(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	fixedNow := time.Unix(1_770_000_000, 0)

	body := &authv1.IntentBody{
		Jti:          "jti-subst",
		IssuedAt:     fixedNow.Unix(),
		SpawnId:      "sp-subst-img",
		Generation:   0,
		TargetNodeId: "",
		Op:           string(intent.OpCreateSpawn),
		Image:        "signed-img@sha256:abc",
	}
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", fixedNow, body, intent.OpCreateSpawn)

	mgr := spawnlet.NewManager(runtime.NewFake(), spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	fs := &fakeCPStream{}
	a := newEnforcedAttacher(t, mgr, fs, ks, AuthModeEnforced, fixedNow)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId:       "sp-subst-img",
		AppRef:        "/unused/app",
		Image:         "evil-img@sha256:bad", // different from signed
		Model:         "m",
		AssertedOwner: "alice",
		Auth:          env,
	})

	phases := fs.phasesFor("sp-subst-img")
	if lastPhase(phases) != nodev1.SpawnPhase_ERROR {
		t.Fatalf("phases = %v, want terminal ERROR on image substitution", phases)
	}
	detail := fs.lastErrorDetail("sp-subst-img")
	if !strings.Contains(detail, string(NACKCorrespondence)) {
		t.Fatalf("ERROR detail = %q, want to contain %s", detail, NACKCorrespondence)
	}
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active != 0 {
		t.Fatalf("active = %d, want 0 (gate must fire before container creation)", active)
	}
}

// ---- (c) valid matching envelope → ACTIVE -------------------------------------------

// TestIntentSeam_ValidEnvelopeActive: a StartSpawn with a correctly signed, matching
// envelope in enforced mode must proceed to SpawnPhase_ACTIVE.
func TestIntentSeam_ValidEnvelopeActive(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	fixedNow := time.Unix(1_770_000_000, 0)
	appDir := writeNodeApp(t)

	// Sign with minimal body: only spawn_id is required for correspondence (optional fields
	// empty → checks skipped in checkStartCorrespondence). assertedOwner matches token accountId.
	body := &authv1.IntentBody{
		Jti:          "jti-valid-seam",
		IssuedAt:     fixedNow.Unix(),
		SpawnId:      "sp-valid-seam",
		Generation:   0,
		TargetNodeId: "",
		Op:           string(intent.OpCreateSpawn),
	}
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", fixedNow, body, intent.OpCreateSpawn)

	be := &scriptedPodBackend{script: scriptGoose}
	fs := &fakeCPStream{}
	a := newEnforcedAttacher(t, newGooseManager(t, be), fs, ks, AuthModeEnforced, fixedNow)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId:       "sp-valid-seam",
		AppRef:        appDir,
		Model:         "m",
		AssertedOwner: "alice",
		Auth:          env,
	})
	defer a.stopSpawn(context.Background(), "sp-valid-seam")

	if got := lastPhase(fs.phasesFor("sp-valid-seam")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("final phase = %v, want ACTIVE for valid matching envelope", got)
	}
}

// ---- (d) forged envelope under AuthModeVerifyLog → ACTIVE ---------------------------

// TestIntentSeam_VerifyLogModeProceedsOnForged: a StartSpawn with a mismatched signed
// intent (image mismatch = CORRESPONDENCE) under AuthModeVerifyLog must still proceed
// to ACTIVE (the failure is logged, not enforced). This proves the mode switch is
// observed at the seam, not just at the unit-verifier level.
func TestIntentSeam_VerifyLogModeProceedsOnForged(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	fixedNow := time.Unix(1_770_000_000, 0)
	appDir := writeNodeApp(t)

	// Sign with a specific (wrong) image so correspondence would fail in enforced mode.
	body := &authv1.IntentBody{
		Jti:          "jti-verifylog-seam",
		IssuedAt:     fixedNow.Unix(),
		SpawnId:      "sp-verifylog-seam",
		Generation:   0,
		TargetNodeId: "",
		Op:           string(intent.OpCreateSpawn),
		Image:        "signed-img@sha256:def", // signed with a specific image
	}
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", fixedNow, body, intent.OpCreateSpawn)

	be := &scriptedPodBackend{script: scriptGoose}
	fs := &fakeCPStream{}
	// AuthModeVerifyLog: failures are logged but not enforced.
	a := newEnforcedAttacher(t, newGooseManager(t, be), fs, ks, AuthModeVerifyLog, fixedNow)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId:       "sp-verifylog-seam",
		AppRef:        appDir,
		Image:         "different-img@sha256:evil", // mismatch — would fail in enforced mode
		Model:         "m",
		AssertedOwner: "alice",
		Auth:          env,
	})
	defer a.stopSpawn(context.Background(), "sp-verifylog-seam")

	// In VerifyLog mode: ACTIVE despite the correspondence mismatch.
	if got := lastPhase(fs.phasesFor("sp-verifylog-seam")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("final phase = %v, want ACTIVE in verify-and-log mode (mismatch must not block)", got)
	}
	// No ERROR status must have been emitted (the log path suppresses NACK returns).
	if hasPhase(fs.phasesFor("sp-verifylog-seam"), nodev1.SpawnPhase_ERROR) {
		t.Fatal("verify-and-log must not emit ERROR status for a correspondence mismatch")
	}
}
