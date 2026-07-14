package node

// intentverify_test.go covers the A4 node-side verification chain [AC1][AM12].
// All tests are hermetic (in-memory certified signer/root, fake clock, no network).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/intent"
)

// ---- helpers -----------------------------------------------------------------

func genECDSA(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type testArtifactSigner struct{ *token.SigningCredential }

func genASKey(t *testing.T) (testArtifactSigner, *token.Verifier) {
	t.Helper()
	fixture := newArtifactFixture(t, time.Unix(1_770_000_000, 0), "prod")
	return testArtifactSigner{fixture.credential}, fixture.verifier
}

// mintNodeToken mints an aud=node token for the given session key and account.
func mintNodeToken(t *testing.T, asPriv testArtifactSigner, _ *token.Verifier, sessionKey *ecdsa.PrivateKey, accountID string, now time.Time) string {
	t.Helper()
	spki, err := x509.MarshalPKIXPublicKey(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	body := &authv1.SessionTokenBody{
		AccountId:      accountID,
		TokenId:        "tok-test",
		Audience:       "node",
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(15 * time.Minute).Unix(),
		SessionKeyHash: token.SessionKeyHash(spki),
		KeyId:          hex.EncodeToString(asPriv.KeyID[:]),
	}
	return mintSessionBody(t, asPriv, body)
}

func mintSessionBody(t *testing.T, signer testArtifactSigner, body *authv1.SessionTokenBody) string {
	t.Helper()
	payload, err := proto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := signer.Sign(token.ArtifactTypeSession, payload)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func buildIntentEnvelope(t *testing.T, asPriv testArtifactSigner, ks *token.Verifier, sessionKey *ecdsa.PrivateKey, accountID string, now time.Time, body *authv1.IntentBody, op intent.Op) *authv1.AuthEnvelope {
	t.Helper()
	nodeToken := mintNodeToken(t, asPriv, ks, sessionKey, accountID, now)
	si, err := intent.Build(op, body, sessionKey)
	if err != nil {
		t.Fatalf("Build SignedIntent: %v", err)
	}
	return &authv1.AuthEnvelope{AccessToken: nodeToken, Intent: si}
}

func makeVerifier(t *testing.T, ks *token.Verifier, nodeOwner, nodeID string, selfHosted bool, now func() time.Time) *IntentVerifier {
	t.Helper()
	return NewIntentVerifier(ks, nodeOwner, nodeID, selfHosted, now)
}

func goodStartFields(spawnID, nodeID string, gen uint64) StartFields {
	return StartFields{
		Op:            intent.OpCreateSpawn,
		SpawnID:       spawnID,
		Generation:    gen,
		AppRef:        "app/ref@sha256:abc",
		Image:         "img@sha256:def",
		Model:         "claude-3",
		AssertedOwner: "alice",
	}
}

func goodStartBody(spawnID, nodeID string, gen uint64, now time.Time) *authv1.IntentBody {
	return &authv1.IntentBody{
		Jti:          "jti-1",
		IssuedAt:     now.Unix(),
		SpawnId:      spawnID,
		Generation:   gen,
		TargetNodeId: nodeID,
		Op:           string(intent.OpCreateSpawn),
		AppRef:       "app/ref@sha256:abc",
		Image:        "img@sha256:def",
		Model:        "claude-3",
	}
}

func TestVerifyExecBindsExactRequestAndSession(t *testing.T) {
	now := time.Unix(1_770_000_000, 0)
	asPriv, artifacts := genASKey(t)
	sessionKey := genECDSA(t)
	baseReq := &authv1.ExecRequest{Argv: []string{"sh", "-lc", "printf exact"}}
	baseFields := ExecFields{
		SpawnID: "sp-exec", Generation: 7, SessionID: "exec-1", AssertedOwner: "alice", Request: baseReq,
	}
	makeEnv := func(jti string, mutate func(*authv1.IntentBody)) *authv1.AuthEnvelope {
		body := &authv1.IntentBody{
			Jti: jti, IssuedAt: now.Unix(), SpawnId: "sp-exec", Generation: 7,
			TargetNodeId: "node-1", SessionId: "exec-1", Op: string(intent.OpExecOpen),
			ExecRequest: proto.Clone(baseReq).(*authv1.ExecRequest),
		}
		if mutate != nil {
			mutate(body)
		}
		return buildIntentEnvelope(t, asPriv, artifacts, sessionKey, "alice", now, body, intent.OpExecOpen)
	}

	t.Run("valid then replay", func(t *testing.T) {
		v := makeVerifier(t, artifacts, "", "node-1", false, func() time.Time { return now })
		env := makeEnv("exec-valid", nil)
		if _, nack, detail := v.VerifyExec(env, baseFields); nack != "" {
			t.Fatalf("valid exec rejected: %s: %s", nack, detail)
		}
		if _, nack, _ := v.VerifyExec(env, baseFields); nack != NACKReplay {
			t.Fatalf("replay nack = %s, want %s", nack, NACKReplay)
		}
	})

	tests := []struct {
		name   string
		fields ExecFields
		mutate func(*authv1.IntentBody)
		want   NACKCode
	}{
		{name: "missing request", fields: ExecFields{SpawnID: "sp-exec", Generation: 7, SessionID: "exec-1", AssertedOwner: "alice"}, want: NACKCorrespondence},
		{name: "missing signed request", fields: baseFields, mutate: func(b *authv1.IntentBody) { b.ExecRequest = nil }, want: NACKCorrespondence},
		{name: "argv substituted", fields: ExecFields{SpawnID: "sp-exec", Generation: 7, SessionID: "exec-1", AssertedOwner: "alice", Request: &authv1.ExecRequest{Argv: []string{"sh", "-lc", "printf evil"}}}, want: NACKCorrespondence},
		{name: "argv reordered", fields: ExecFields{SpawnID: "sp-exec", Generation: 7, SessionID: "exec-1", AssertedOwner: "alice", Request: &authv1.ExecRequest{Argv: []string{"-lc", "sh", "printf exact"}}}, want: NACKCorrespondence},
		{name: "generation mismatch", fields: ExecFields{SpawnID: "sp-exec", Generation: 8, SessionID: "exec-1", AssertedOwner: "alice", Request: baseReq}, want: NACKCorrespondence},
		{name: "owner mismatch", fields: ExecFields{SpawnID: "sp-exec", Generation: 7, SessionID: "exec-1", AssertedOwner: "mallory", Request: baseReq}, want: NACKOwnerMismatch},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := makeVerifier(t, artifacts, "", "node-1", false, func() time.Time { return now })
			if _, nack, _ := v.VerifyExec(makeEnv(fmt.Sprintf("exec-bad-%d", i), tt.mutate), tt.fields); nack != tt.want {
				t.Fatalf("nack = %s, want %s", nack, tt.want)
			}
		})
	}
}

// ---- tests -------------------------------------------------------------------

func TestVerifyStartHappyPath(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	body := goodStartBody("sp-1", "node-1", 1, now)
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpCreateSpawn)

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	fields.AssertedOwner = "alice"

	_, nack, detail := v.VerifyStart(env, fields)
	if nack != "" {
		t.Fatalf("expected success, got nack=%s detail=%s", nack, detail)
	}
}

// ---- correspondence negatives [AC1] ----------------------------------------

// Substituted image must be refused.
func TestCorrespondenceSubstitutedImageRefused(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	body := goodStartBody("sp-1", "node-1", 1, now)
	body.Image = "img@sha256:def" // the signed image
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpCreateSpawn)

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	fields.Image = "malicious@sha256:evil" // CP substituted image

	_, nack, _ := v.VerifyStart(env, fields)
	if nack != NACKCorrespondence {
		t.Fatalf("substituted image: want NACKCorrespondence, got %q", nack)
	}
}

func TestCorrespondenceSubstitutedAttachedSecretSetRefused(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	body := goodStartBody("sp-1", "node-1", 1, now)
	body.AttachedSecretIds = []string{"manifest-secret", "selected-secret"}
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpCreateSpawn)
	fields := goodStartFields("sp-1", "node-1", 1)
	fields.AttachedSecretIDs = []string{"manifest-secret", "substituted-secret"}
	v := makeVerifier(t, ks, "alice", "node-1", false, func() time.Time { return now })
	if _, nack, _ := v.VerifyStart(env, fields); nack != NACKCorrespondence {
		t.Fatalf("substituted attached secrets: got %q, want %q", nack, NACKCorrespondence)
	}
}

// Substituted target_node_id (different node) must be refused.
func TestCorrespondenceSubstitutedTargetRefused(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	body := goodStartBody("sp-1", "node-original", 1, now) // intent targets node-original
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpCreateSpawn)

	v := makeVerifier(t, ks, "alice", "node-different", false, clock) // but verifier is on node-different
	fields := goodStartFields("sp-1", "node-different", 1)

	_, nack, _ := v.VerifyStart(env, fields)
	if nack != NACKCorrespondence {
		t.Fatalf("substituted target: want NACKCorrespondence, got %q", nack)
	}
}

// Substituted generation must be refused.
func TestCorrespondenceSubstitutedGenerationRefused(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	body := goodStartBody("sp-1", "node-1", 1, now) // signed generation = 1
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpCreateSpawn)

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	fields.Generation = 2 // CP claims a different generation

	_, nack, _ := v.VerifyStart(env, fields)
	if nack != NACKCorrespondence {
		t.Fatalf("substituted generation: want NACKCorrespondence, got %q", nack)
	}
}

// ---- replay / freshness [AC1] -----------------------------------------------

// Cross-restart jti: an intent issued before process start must be refused.
func TestCrossRestartJTIRefused(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)

	// Process "started" at T0+10s; intent was issued at T0 (before start).
	processStart := time.Unix(1_770_000_000, 0).Add(10 * time.Second)
	intentIssuedAt := time.Unix(1_770_000_000, 0) // before process start

	clock := func() time.Time { return processStart.Add(5 * time.Second) } // now = T0+15s

	body := goodStartBody("sp-1", "node-1", 1, intentIssuedAt)
	body.Jti = "jti-pre-start"
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", processStart, body, intent.OpCreateSpawn)

	// Build verifier with jtiCache seeded at processStart (cross-restart test).
	v := &IntentVerifier{
		artifacts:  ks,
		nodeOwner:  "alice",
		nodeID:     "node-1",
		selfHosted: false,
		now:        clock,
		jtiCache:   intent.NewJTICache(func() time.Time { return processStart }),
	}

	fields := goodStartFields("sp-1", "node-1", 1)
	_, nack, detail := v.VerifyStart(env, fields)
	if nack != NACKReplay && nack != NACKStale {
		// Either REPLAY (jti predates process start) or STALE (too old) is acceptable —
		// the freshness check (step 7) runs before jticache (step 8) when the age is large.
		t.Fatalf("cross-restart jti: want REPLAY or STALE, got nack=%q detail=%s", nack, detail)
	}
}

// Duplicate jti must be refused on second admission.
func TestDuplicateJTIRefused(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	body := goodStartBody("sp-1", "node-1", 1, now)
	body.Jti = "jti-dup"
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpCreateSpawn)
	fields := goodStartFields("sp-1", "node-1", 1)

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)

	// First admission must succeed.
	if _, nack, detail := v.VerifyStart(env, fields); nack != "" {
		t.Fatalf("first: want success, got %s: %s", nack, detail)
	}

	// Second admission of same jti must be refused.
	_, nack, _ := v.VerifyStart(env, fields)
	if nack != NACKReplay {
		t.Fatalf("duplicate jti: want NACKReplay, got %q", nack)
	}
}

// Skew rejection must return the node's own time in the detail [AC1 minor note].
func TestSkewRejectionReturnsNodeTime(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)

	nodeNow := time.Unix(1_770_001_000, 0)
	clock := func() time.Time { return nodeNow }

	// Intent issued well beyond SkewBudget in the future (spec §5: future tolerance = SkewBudget only).
	futureIssuedAt := nodeNow.Add(intent.SkewBudget + time.Minute)
	body := goodStartBody("sp-1", "node-1", 1, futureIssuedAt)
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", nodeNow, body, intent.OpCreateSpawn)

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	_, nack, detail := v.VerifyStart(env, fields)
	if nack != NACKSkew {
		t.Fatalf("future intent beyond SkewBudget: want NACKSkew, got %q", nack)
	}
	// Node time must appear in the detail.
	nodeTimeStr := "1770001000"
	if detail == "" || !containsStr(detail, nodeTimeStr) {
		t.Fatalf("skew detail should contain node time %s: got %q", nodeTimeStr, detail)
	}
}

// Future intent within SkewBudget must be accepted [AC1][spec §5].
func TestFutureIntentWithinSkewBudgetAccepted(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)

	nodeNow := time.Unix(1_770_001_000, 0)
	clock := func() time.Time { return nodeNow }

	// Issued 1s less than SkewBudget in the future — should pass.
	futureIssuedAt := nodeNow.Add(intent.SkewBudget - time.Second)
	body := goodStartBody("sp-1", "node-1", 1, futureIssuedAt)
	body.Jti = "jti-future-ok"
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", nodeNow, body, intent.OpCreateSpawn)

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	_, nack, detail := v.VerifyStart(env, fields)
	if nack != "" {
		t.Fatalf("future intent within SkewBudget: want success, got nack=%s detail=%s", nack, detail)
	}
}

// Future intent beyond SkewBudget (but within old FreshnessWindow+SkewBudget) must be rejected [spec §5].
func TestFutureIntentBeyondSkewBudgetRejected(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)

	nodeNow := time.Unix(1_770_001_000, 0)
	clock := func() time.Time { return nodeNow }

	// Issued SkewBudget+1s in the future — outside the ±30s spec tolerance.
	futureIssuedAt := nodeNow.Add(intent.SkewBudget + time.Second)
	body := goodStartBody("sp-1", "node-1", 1, futureIssuedAt)
	body.Jti = "jti-future-bad"
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", nodeNow, body, intent.OpCreateSpawn)

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	_, nack, _ := v.VerifyStart(env, fields)
	if nack != NACKSkew {
		t.Fatalf("future intent beyond SkewBudget: want NACKSkew, got %q", nack)
	}
}

// Empty asserted_owner in enforced non-self-hosted (cloud) mode must be refused [review finding].
func TestEnforcedCloudModeRejectsEmptyAssertedOwner(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	body := goodStartBody("sp-1", "node-1", 1, now)
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpCreateSpawn)

	// Enforced non-self-hosted verifier with empty assertedOwner.
	v := makeVerifier(t, ks, "", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	fields.AssertedOwner = "" // no asserted owner from CP

	_, nack, _ := v.VerifyStart(env, fields)
	if nack != NACKOwnerMismatch {
		t.Fatalf("empty asserted_owner in enforced cloud mode: want NACKOwnerMismatch, got %q", nack)
	}
}

// Empty asserted_owner in enforced self-hosted mode must be tolerated (NodeOwner covers it) [spec §5].
func TestSelfHostedToleratesEmptyAssertedOwner(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	body := goodStartBody("sp-1", "node-1", 1, now)
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpCreateSpawn)

	// Self-hosted verifier: assertedOwner may be empty; nodeOwner check covers it.
	v := makeVerifier(t, ks, "alice", "node-1", true, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	fields.AssertedOwner = "" // intentionally empty

	_, nack, detail := v.VerifyStart(env, fields)
	if nack != "" {
		t.Fatalf("empty asserted_owner in self-hosted mode: want success, got nack=%s detail=%s", nack, detail)
	}
}

// CNF mismatch: SPKI does not hash to session_key_hash in the token [AM11].
func TestCNFMismatch(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	differentKey := genECDSA(t) // different SPKI will be signed into intent
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	// Token's cnf is bound to sessionKey, but intent will use differentKey's SPKI.
	spki, _ := x509.MarshalPKIXPublicKey(&sessionKey.PublicKey)
	cnfHash := sha256.Sum256(spki)
	tokenBody := &authv1.SessionTokenBody{
		AccountId:      "alice",
		TokenId:        "tok-cnf-test",
		Audience:       "node",
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(15 * time.Minute).Unix(),
		SessionKeyHash: cnfHash[:],
		KeyId:          hex.EncodeToString(asPriv.KeyID[:]),
	}
	nodeTok := mintSessionBody(t, asPriv, tokenBody)

	// Sign the intent with differentKey (SPKI won't match token's cnf).
	body := goodStartBody("sp-1", "node-1", 1, now)
	si, _ := intent.Build(intent.OpCreateSpawn, body, differentKey)
	env := &authv1.AuthEnvelope{AccessToken: nodeTok, Intent: si}

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	_, nack, _ := v.VerifyStart(env, fields)
	if nack != NACKCNFMismatch {
		t.Fatalf("CNF mismatch: want NACKCNFMismatch, got %q", nack)
	}
}

// Wrong audience must be refused [MC2].
func TestWrongAudienceRefused(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	// Mint a token with aud=cp (not aud=node).
	spki, _ := x509.MarshalPKIXPublicKey(&sessionKey.PublicKey)
	tokenBody := &authv1.SessionTokenBody{
		AccountId:      "alice",
		TokenId:        "tok-cp",
		Audience:       "cp", // wrong audience
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(15 * time.Minute).Unix(),
		SessionKeyHash: token.SessionKeyHash(spki),
		KeyId:          hex.EncodeToString(asPriv.KeyID[:]),
	}
	cpTok := mintSessionBody(t, asPriv, tokenBody)

	body := goodStartBody("sp-1", "node-1", 1, now)
	si, _ := intent.Build(intent.OpCreateSpawn, body, sessionKey)
	env := &authv1.AuthEnvelope{AccessToken: cpTok, Intent: si}

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	_, nack, _ := v.VerifyStart(env, fields)
	if nack != NACKWrongAudience {
		t.Fatalf("wrong aud: want NACKWrongAudience, got %q", nack)
	}
}

// Self-hosted mode enforces account_id == NodeOwner [§5].
func TestSelfHostedOwnerEnforcement(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	// Token says account_id=alice; node is owned by alice.
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now,
		goodStartBody("sp-1", "node-1", 1, now), intent.OpCreateSpawn)

	// Self-hosted verifier also owned by alice -> should pass.
	v := makeVerifier(t, ks, "alice", "node-1", true, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	if _, nack, _ := v.VerifyStart(env, fields); nack != "" {
		t.Fatalf("self-hosted same-owner: want success, got %q", nack)
	}

	// Self-hosted verifier owned by bob but token says alice -> should fail.
	v2 := makeVerifier(t, ks, "bob", "node-1", true, clock)
	_, nack, _ := v2.VerifyStart(env, fields)
	if nack != NACKOwnerMismatch {
		t.Fatalf("self-hosted different-owner: want NACKOwnerMismatch, got %q", nack)
	}
}

// Empty execution field vs non-empty signed field must be caught [AM1 hardening].
// A CP that sends an empty image/model/app_ref while the client signed a non-empty value
// must be rejected — the guard is on the signed (body) value, not the executed (fields) value.
func TestCorrespondenceSignedNonEmptyExecEmptyRefused(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	body := goodStartBody("sp-1", "node-1", 1, now)
	body.Image = "img@sha256:signed" // signed a specific image
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpCreateSpawn)

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	fields.Image = "" // CP sends empty — previously would skip the check, now must fail

	_, nack, _ := v.VerifyStart(env, fields)
	if nack != NACKCorrespondence {
		t.Fatalf("signed non-empty vs exec empty: want NACKCorrespondence, got %q", nack)
	}
}

// Nil AuthEnvelope in enforced mode must return NACKMissingIntent.
func TestNilEnvelopeEnforcedMode(t *testing.T) {
	_, ks := genASKey(t)
	clock := func() time.Time { return time.Unix(1_770_000_000, 0) }
	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	_, nack, _ := v.VerifyStart(nil, goodStartFields("sp-1", "node-1", 1))
	if nack != NACKMissingIntent {
		t.Fatalf("nil envelope enforced: want NACKMissingIntent, got %q", nack)
	}
}

// VerifyOpen happy path.
func TestVerifyOpenHappyPath(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	body := &authv1.IntentBody{
		Jti:          "jti-open-1",
		IssuedAt:     now.Unix(),
		SpawnId:      "sp-1",
		Generation:   1,
		TargetNodeId: "node-1",
		Op:           string(intent.OpSessionOpen),
		SessionId:    "sess-a",
	}
	spki, _ := x509.MarshalPKIXPublicKey(&sessionKey.PublicKey)
	tokenBody := &authv1.SessionTokenBody{
		AccountId:      "alice",
		TokenId:        "tok-open",
		Audience:       "node",
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(15 * time.Minute).Unix(),
		SessionKeyHash: token.SessionKeyHash(spki),
		KeyId:          hex.EncodeToString(asPriv.KeyID[:]),
	}
	nodeTok := mintSessionBody(t, asPriv, tokenBody)
	si, _ := intent.Build(intent.OpSessionOpen, body, sessionKey)
	env := &authv1.AuthEnvelope{AccessToken: nodeTok, Intent: si}

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := OpenFields{SpawnID: "sp-1", Generation: 1, SessionID: "sess-a", AssertedOwner: "alice"}
	auth, nack, detail := v.VerifyOpen(env, fields)
	if nack != "" {
		t.Fatalf("open happy path: want success, got nack=%s detail=%s", nack, detail)
	}
	if auth.AccountID != "alice" || auth.TokenID != "tok-open" || auth.IssuedAt != now.Unix() || !auth.ExpiresAt.Equal(now.Add(15*time.Minute)) || len(auth.SessionKeyHash) != sha256.Size {
		t.Fatalf("authorization = %+v", auth)
	}
}

type cutoffUserRevocationLookup struct {
	explicitToken string
	accountID     string
	cutoff        int64
}

func (r cutoffUserRevocationLookup) IsRevoked(tokenID, accountID string, issuedAt int64) bool {
	return tokenID == r.explicitToken || (accountID == r.accountID && issuedAt < r.cutoff)
}

func TestIntentVerifierEnforcesExplicitTokensAndExclusiveAccountCutoffForEveryOperation(t *testing.T) {
	now := time.Unix(1_770_000_000, 0)
	operations := []struct {
		name string
		op   intent.Op
	}{{"start", intent.OpCreateSpawn}, {"open", intent.OpSessionOpen}, {"reauth", intent.OpSessionReauth}}
	cases := []struct {
		name          string
		issuedAt      time.Time
		explicitToken string
		wantRejected  bool
	}{
		{name: "explicit", issuedAt: now.Add(time.Second), explicitToken: "tok-test", wantRejected: true},
		{name: "before cutoff", issuedAt: now.Add(-time.Second), wantRejected: true},
		{name: "at cutoff", issuedAt: now},
		{name: "after cutoff", issuedAt: now.Add(time.Second)},
	}
	for _, operation := range operations {
		for _, test := range cases {
			t.Run(operation.name+"/"+test.name, func(t *testing.T) {
				signer, artifacts := genASKey(t)
				sessionKey := genECDSA(t)
				var body *authv1.IntentBody
				switch operation.op {
				case intent.OpCreateSpawn:
					body = goodStartBody("sp-1", "node-1", 1, now)
				case intent.OpSessionOpen:
					body = &authv1.IntentBody{Jti: "jti-open", IssuedAt: now.Unix(), Op: string(operation.op), SpawnId: "sp-1", Generation: 1, SessionId: "sess-a", TargetNodeId: "node-1"}
				case intent.OpSessionReauth:
					body = &authv1.IntentBody{Jti: "jti-reauth", IssuedAt: now.Unix(), Op: string(operation.op), SpawnId: "sp-1", Generation: 1, SessionId: "sess-a", TargetNodeId: "node-1", NewTokenId: "tok-test"}
				}
				env := buildIntentEnvelope(t, signer, artifacts, sessionKey, "alice", test.issuedAt, body, operation.op)
				lookup := cutoffUserRevocationLookup{explicitToken: test.explicitToken, accountID: "alice", cutoff: now.Unix()}
				verifier := NewIntentVerifier(artifacts, "alice", "node-1", false, func() time.Time { return now }, lookup)
				var nack NACKCode
				switch operation.op {
				case intent.OpCreateSpawn:
					_, nack, _ = verifier.VerifyStart(env, goodStartFields("sp-1", "node-1", 1))
				case intent.OpSessionOpen:
					_, nack, _ = verifier.VerifyOpen(env, OpenFields{SpawnID: "sp-1", Generation: 1, SessionID: "sess-a", AssertedOwner: "alice"})
				case intent.OpSessionReauth:
					_, nack, _ = verifier.VerifyReauth(env, ReauthFields{SpawnID: "sp-1", Generation: 1, SessionID: "sess-a", AssertedOwner: "alice"})
				}
				if gotRejected := nack == NACKTokenInvalid; gotRejected != test.wantRejected {
					t.Fatalf("nack=%q rejected=%v want=%v", nack, gotRejected, test.wantRejected)
				}
			})
		}
	}
}

func TestVerifyReauthRequiresReplacementTokenID(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	body := &authv1.IntentBody{
		Jti: "jti-reauth", IssuedAt: now.Unix(), SpawnId: "sp-1", Generation: 1,
		TargetNodeId: "node-1", Op: string(intent.OpSessionReauth), SessionId: "sess-a",
		NewTokenId: "wrong-token",
	}
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpSessionReauth)
	v := makeVerifier(t, ks, "alice", "node-1", false, func() time.Time { return now })
	_, nack, _ := v.VerifyReauth(env, ReauthFields{SpawnID: "sp-1", Generation: 1, SessionID: "sess-a", AssertedOwner: "alice"})
	if nack != NACKCorrespondence {
		t.Fatalf("replacement token mismatch = %q", nack)
	}
}

func TestVerifyStartRejectsWrongOperationDomain(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	body := goodStartBody("sp-1", "node-1", 1, now)
	body.Op = string(intent.OpResumeSpawn)
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpResumeSpawn)
	v := makeVerifier(t, ks, "alice", "node-1", false, func() time.Time { return now })
	_, nack, _ := v.VerifyStart(env, goodStartFields("sp-1", "node-1", 1))
	if nack != NACKCorrespondence {
		t.Fatalf("wrong operation = %q", nack)
	}
}

func TestVerifyStartAcceptsExactLifecycleOperation(t *testing.T) {
	for _, op := range []intent.Op{
		intent.OpCreateSpawn, intent.OpResumeSpawn, intent.OpRecreateSpawn,
		intent.OpMigrateSpawn, intent.OpForkSpawn,
	} {
		t.Run(string(op), func(t *testing.T) {
			asPriv, ks := genASKey(t)
			sessionKey := genECDSA(t)
			now := time.Unix(1_770_000_000, 0)
			body := goodStartBody("sp-1", "node-1", 1, now)
			body.Op = string(op)
			env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, op)
			fields := goodStartFields("sp-1", "node-1", 1)
			fields.Op = op
			auth, nack, detail := makeVerifier(t, ks, "alice", "node-1", false, func() time.Time { return now }).VerifyStart(env, fields)
			if nack != "" || auth.AccountID != "alice" {
				t.Fatalf("authorization=%+v nack=%s detail=%s", auth, nack, detail)
			}
		})
	}
}

// Helper: check if s contains sub.
func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}

// Ensure the proto import is used (compile-time guard).
var _ = proto.Marshal
