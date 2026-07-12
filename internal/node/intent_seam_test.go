package node

// intent_seam_test.go: end-to-end enforcement tests at the node seam [AC1][AM12].
//
// These tests drive the production startSpawn path with an enforced IntentVerifier so we
// can assert that:
//   (a) a StartSpawn with nil Auth → SpawnPhase_ERROR with MISSING_INTENT (no container)
//   (b) a StartSpawn with a substituted image → ERROR with CORRESPONDENCE (no container)
//   (c) a valid matching envelope → SpawnPhase_ACTIVE (container created, agent handshakes)
//
// Tests (a)/(b) verify the gate is actually wired: the verifier runs BEFORE mgr.CreateWithSelection
// so a.active == 0 proves no container was created.
// Test (c) exercises the full lifecycle using scriptedPodBackend + scriptGoose.

import (
	"context"
	"crypto/ecdsa"
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

func (f *fakeCPStream) lastSessionAuthClosed() *nodev1.SessionAuthClosed {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if closed := f.sent[i].GetSessionAuthClosed(); closed != nil {
			return closed
		}
	}
	return nil
}

func (f *fakeCPStream) sessionAuthClosedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var count int
	for _, msg := range f.sent {
		if msg.GetSessionAuthClosed() != nil {
			count++
		}
	}
	return count
}

// newEnforcedAttacher builds an attacher with an enforced IntentVerifier (selfHosted=false,
// nodeOwner="", so only assertedOwner is validated). The verifier uses a fixed clock.
func newEnforcedAttacher(t *testing.T, mgr *spawnlet.Manager, fs cpStream, ks *token.Verifier, fixedNow time.Time) *attacher {
	t.Helper()
	a := newAttacher(mgr, fs)
	a.verifier = NewIntentVerifier(ks, "", "", false, func() time.Time { return fixedNow })
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
	a := newEnforcedAttacher(t, mgr, fs, ks, fixedNow)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId:       "sp-nil-auth",
		AppRef:        "/unused/app", // never reached
		Model:         "m",
		AssertedOwner: "alice",
		Auth:          nil,
		IntentOp:      string(intent.OpCreateSpawn),
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
	a := newEnforcedAttacher(t, mgr, fs, ks, fixedNow)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId:       "sp-subst-img",
		AppRef:        "/unused/app",
		Image:         "evil-img@sha256:bad", // different from signed
		Model:         "m",
		AssertedOwner: "alice",
		Auth:          env,
		IntentOp:      string(intent.OpCreateSpawn),
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
		AppRef:       appDir,
		Model:        "m",
	}
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", fixedNow, body, intent.OpCreateSpawn)

	be := &scriptedPodBackend{script: scriptGoose}
	fs := &fakeCPStream{}
	a := newEnforcedAttacher(t, newGooseManager(t, be), fs, ks, fixedNow)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId:       "sp-valid-seam",
		AppRef:        appDir,
		Model:         "m",
		AssertedOwner: "alice",
		Auth:          env,
		IntentOp:      string(intent.OpCreateSpawn),
	})
	defer a.stopSpawn(context.Background(), "sp-valid-seam")

	if got := lastPhase(fs.phasesFor("sp-valid-seam")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("final phase = %v, want ACTIVE for valid matching envelope", got)
	}
	owner, generation, ok := a.mgr.SpawnOwnerGeneration("sp-valid-seam")
	if !ok || owner != "alice" || generation != 0 {
		t.Fatalf("live owner snapshot = %q/%d/%v", owner, generation, ok)
	}
}

func TestSessionOpenRejectionEmitsAddressedAuthClosed(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	appDir := writeNodeApp(t)
	startBody := &authv1.IntentBody{
		Jti: "start", IssuedAt: now.Unix(), SpawnId: "sp-open-reject", TargetNodeId: "node-1",
		Op: string(intent.OpCreateSpawn), AppRef: appDir, Model: "m",
	}
	startEnv := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, startBody, intent.OpCreateSpawn)
	fs := &fakeCPStream{}
	a := newEnforcedAttacher(t, newGooseManager(t, &scriptedPodBackend{script: scriptGoose}), fs, ks, now)
	a.cfg.NodeID = "node-1"
	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: "sp-open-reject", AppRef: appDir, Model: "m", AssertedOwner: "alice",
		Auth: startEnv, IntentOp: string(intent.OpCreateSpawn),
	})
	defer a.stopSpawn(context.Background(), "sp-open-reject")

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{
		SpawnId: "sp-open-reject", Generation: 0, SessionId: "session-7", ClientId: "client-9",
		AssertedOwner: "mallory", AttachmentId: "attachment-9",
	}}})
	closed := fs.lastSessionAuthClosed()
	if closed == nil || closed.GetSpawnId() != "sp-open-reject" || closed.GetGeneration() != 0 ||
		closed.GetSessionId() != "session-7" || closed.GetClientId() != "client-9" ||
		closed.GetAttachmentId() != "attachment-9" ||
		closed.GetReason() != "live spawn ownership or generation mismatch" {
		t.Fatalf("SessionAuthClosed = %+v", closed)
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{
		SpawnId: "sp-open-reject", Generation: 0, SessionId: "session-8", ClientId: "client-10",
		AssertedOwner: "alice", AttachmentId: "attachment-10",
	}}})
	closed = fs.lastSessionAuthClosed()
	if closed == nil || closed.GetSessionId() != "session-8" || closed.GetClientId() != "client-10" ||
		closed.GetAttachmentId() != "attachment-10" ||
		closed.GetReason() != "MISSING_INTENT: no auth envelope" {
		t.Fatalf("verification SessionAuthClosed = %+v", closed)
	}
}

func TestSessionReauthUnknownAttachmentClosesExactAddress(t *testing.T) {
	a, fs, signer, verifier, sessionKey, now := newReauthSeam(t)
	body := &authv1.IntentBody{
		Jti: "reauth-unknown", IssuedAt: now.Unix(), SpawnId: "sp-reauth", Generation: 1,
		Op: string(intent.OpSessionReauth), SessionId: "session-1", NewTokenId: "tok-test",
	}
	a.reauthenticateClient(&nodev1.SessionReauth{
		SpawnId: "sp-reauth", Generation: 1, SessionId: "session-1", ClientId: "client-1",
		AssertedOwner: "alice", AttachmentId: "attachment-unknown",
		Auth: buildIntentEnvelope(t, signer, verifier, sessionKey, "alice", now, body, intent.OpSessionReauth),
	})
	closed := fs.lastSessionAuthClosed()
	if closed == nil || closed.GetAttachmentId() != "attachment-unknown" || closed.GetClientId() != "client-1" ||
		closed.GetReason() != "session reauthentication attachment not found" {
		t.Fatalf("SessionAuthClosed = %+v", closed)
	}
}

func TestSessionReauthStaleAttachmentDoesNotCloseReplacement(t *testing.T) {
	a, fs, signer, verifier, sessionKey, now := newReauthSeam(t)
	key := sessionAuthKey{spawnID: "sp-reauth", sessionID: "session-1", clientID: "client-1"}
	a.auths.register(key, sessionAuthRecord{
		accountID: "alice", tokenID: "current", expiresAt: time.Now().Add(time.Hour), sessionKeyHash: []byte("key"),
		generation: 1, nodeID: "node-1", attachmentID: "attachment-current",
	}, func(string) { t.Fatal("stale reauth closed replacement") })
	body := &authv1.IntentBody{
		Jti: "reauth-stale", IssuedAt: now.Unix(), SpawnId: "sp-reauth", Generation: 1,
		Op: string(intent.OpSessionReauth), SessionId: "session-1", NewTokenId: "tok-test",
	}
	a.reauthenticateClient(&nodev1.SessionReauth{
		SpawnId: "sp-reauth", Generation: 1, SessionId: "session-1", ClientId: "client-1",
		AssertedOwner: "alice", AttachmentId: "attachment-stale",
		Auth: buildIntentEnvelope(t, signer, verifier, sessionKey, "alice", now, body, intent.OpSessionReauth),
	})
	if fs.sessionAuthClosedCount() != 0 {
		t.Fatal("stale reauth emitted an addressed close")
	}
	if attachment, ok := a.auths.attachment(key); !ok || attachment != "attachment-current" {
		t.Fatalf("replacement attachment = %q/%v", attachment, ok)
	}
}

func TestSessionReauthInvalidVerificationStillClosesExactAddress(t *testing.T) {
	a, fs, _, _, _, _ := newReauthSeam(t)
	a.reauthenticateClient(&nodev1.SessionReauth{
		SpawnId: "sp-reauth", Generation: 1, SessionId: "session-1", ClientId: "client-1",
		AssertedOwner: "alice", AttachmentId: "attachment-invalid",
	})
	closed := fs.lastSessionAuthClosed()
	if closed == nil || closed.GetAttachmentId() != "attachment-invalid" || closed.GetReason() != "MISSING_INTENT: no auth envelope" {
		t.Fatalf("SessionAuthClosed = %+v", closed)
	}
}

func TestLateOlderSessionOpenIsIgnoredBeforeInvalidVerification(t *testing.T) {
	a, fs, _, _, _, _ := newReauthSeam(t)
	key := sessionAuthKey{spawnID: "sp-reauth", sessionID: "session-1", clientID: "client-1"}
	a.auths.register(key, sessionAuthRecord{
		accountID: "alice", expiresAt: time.Now().Add(time.Hour), attachmentID: "attachment-current", attachmentSequence: 2,
	}, func(string) { t.Fatal("stale invalid open closed replacement") })
	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{
		SpawnId: "sp-reauth", Generation: 1, SessionId: "session-1", ClientId: "client-1",
		AssertedOwner: "alice", AttachmentId: "attachment-stale", AttachmentSequence: 1,
	}}})
	if fs.sessionAuthClosedCount() != 0 {
		t.Fatal("stale invalid open emitted close")
	}
	if attachment, ok := a.auths.attachment(key); !ok || attachment != "attachment-current" {
		t.Fatalf("replacement attachment = %q/%v", attachment, ok)
	}
}

func newReauthSeam(t *testing.T) (*attacher, *fakeCPStream, testArtifactSigner, *token.Verifier, *ecdsa.PrivateKey, time.Time) {
	t.Helper()
	now := time.Unix(1_770_000_000, 0)
	signer, verifier := genASKey(t)
	sessionKey := genECDSA(t)
	mgr := newGooseManager(t, &scriptedPodBackend{script: scriptGoose})
	if _, err := mgr.CreateAuthorizedWithSelection(context.Background(), "sp-reauth", writeNodeApp(t), "m", "", "", 1, "alice", spawnlet.AgentSelection{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Stop(context.Background(), "sp-reauth") })
	fs := &fakeCPStream{}
	a := newEnforcedAttacher(t, mgr, fs, verifier, now)
	return a, fs, signer, verifier, sessionKey, now
}
