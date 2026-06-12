package node

// intentverify_test.go covers the A4 node-side verification chain [AC1][AM12].
// All tests are hermetic (in-memory key set, fake clock, no network).

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
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

func genASKey(t *testing.T) (ed25519.PrivateKey, token.KeySet) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := token.NewKeySet(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return priv, ks
}

// mintNodeToken mints an aud=node token for the given session key and account.
func mintNodeToken(t *testing.T, asPriv ed25519.PrivateKey, ks token.KeySet, sessionKey *ecdsa.PrivateKey, accountID string, now time.Time) string {
	t.Helper()
	spki, err := x509.MarshalPKIXPublicKey(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := token.KeyID(asPriv.Public().(ed25519.PublicKey))
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
		KeyId:          keyID,
	}
	wire, err := token.Mint(body, asPriv)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func buildIntentEnvelope(t *testing.T, asPriv ed25519.PrivateKey, ks token.KeySet, sessionKey *ecdsa.PrivateKey, accountID string, now time.Time, body *authv1.IntentBody, op intent.Op) *authv1.AuthEnvelope {
	t.Helper()
	nodeToken := mintNodeToken(t, asPriv, ks, sessionKey, accountID, now)
	si, err := intent.Build(op, body, sessionKey)
	if err != nil {
		t.Fatalf("Build SignedIntent: %v", err)
	}
	return &authv1.AuthEnvelope{AccessToken: nodeToken, Intent: si}
}

func makeVerifier(t *testing.T, ks token.KeySet, nodeOwner, nodeID string, selfHosted bool, now func() time.Time) *IntentVerifier {
	t.Helper()
	return NewIntentVerifier(ks, nodeOwner, nodeID, selfHosted, AuthModeEnforced, now)
}

func goodStartFields(spawnID, nodeID string, gen uint64) StartFields {
	return StartFields{
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

	nack, detail := v.VerifyStart(env, fields)
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

	nack, _ := v.VerifyStart(env, fields)
	if nack != NACKCorrespondence {
		t.Fatalf("substituted image: want NACKCorrespondence, got %q", nack)
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

	nack, _ := v.VerifyStart(env, fields)
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

	nack, _ := v.VerifyStart(env, fields)
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
		keySet:     ks,
		nodeOwner:  "alice",
		nodeID:     "node-1",
		selfHosted: false,
		mode:       AuthModeEnforced,
		now:        clock,
		jtiCache:   intent.NewJTICache(func() time.Time { return processStart }),
	}

	fields := goodStartFields("sp-1", "node-1", 1)
	nack, detail := v.VerifyStart(env, fields)
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
	if nack, detail := v.VerifyStart(env, fields); nack != "" {
		t.Fatalf("first: want success, got %s: %s", nack, detail)
	}

	// Second admission of same jti must be refused.
	nack, _ := v.VerifyStart(env, fields)
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
	nack, detail := v.VerifyStart(env, fields)
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
	nack, detail := v.VerifyStart(env, fields)
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
	nack, _ := v.VerifyStart(env, fields)
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

	nack, _ := v.VerifyStart(env, fields)
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

	nack, detail := v.VerifyStart(env, fields)
	if nack != "" {
		t.Fatalf("empty asserted_owner in self-hosted mode: want success, got nack=%s detail=%s", nack, detail)
	}
}

// Insecure (verify-and-log) mode must log the failure but NOT enforce it [AM12].
func TestInsecureModeLogsNotEnforces(t *testing.T) {
	asPriv, ks := genASKey(t)
	sessionKey := genECDSA(t)
	now := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return now }

	// Use a WRONG image in correspondence so verification would fail in enforced mode.
	body := goodStartBody("sp-1", "node-1", 1, now)
	body.Image = "img@sha256:signed"
	env := buildIntentEnvelope(t, asPriv, ks, sessionKey, "alice", now, body, intent.OpCreateSpawn)

	v := NewIntentVerifier(ks, "alice", "node-1", false, AuthModeVerifyLog, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	fields.Image = "img@sha256:SUBSTITUTED" // correspondence mismatch

	// In verify-and-log mode: no NACK returned even though correspondence fails.
	nack, _ := v.VerifyStart(env, fields)
	if nack != "" {
		t.Fatalf("verify-and-log mode must not return NACK, got %q", nack)
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
	keyID, _ := token.KeyID(asPriv.Public().(ed25519.PublicKey))
	cnfHash := sha256.Sum256(spki)
	tokenBody := &authv1.SessionTokenBody{
		AccountId:      "alice",
		TokenId:        "tok-cnf-test",
		Audience:       "node",
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(15 * time.Minute).Unix(),
		SessionKeyHash: cnfHash[:],
		KeyId:          keyID,
	}
	nodeTok, _ := token.Mint(tokenBody, asPriv)

	// Sign the intent with differentKey (SPKI won't match token's cnf).
	body := goodStartBody("sp-1", "node-1", 1, now)
	si, _ := intent.Build(intent.OpCreateSpawn, body, differentKey)
	env := &authv1.AuthEnvelope{AccessToken: nodeTok, Intent: si}

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	nack, _ := v.VerifyStart(env, fields)
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
	keyID, _ := token.KeyID(asPriv.Public().(ed25519.PublicKey))
	tokenBody := &authv1.SessionTokenBody{
		AccountId:      "alice",
		TokenId:        "tok-cp",
		Audience:       "cp", // wrong audience
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(15 * time.Minute).Unix(),
		SessionKeyHash: token.SessionKeyHash(spki),
		KeyId:          keyID,
	}
	cpTok, _ := token.Mint(tokenBody, asPriv)

	body := goodStartBody("sp-1", "node-1", 1, now)
	si, _ := intent.Build(intent.OpCreateSpawn, body, sessionKey)
	env := &authv1.AuthEnvelope{AccessToken: cpTok, Intent: si}

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := goodStartFields("sp-1", "node-1", 1)
	nack, _ := v.VerifyStart(env, fields)
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
	if nack, _ := v.VerifyStart(env, fields); nack != "" {
		t.Fatalf("self-hosted same-owner: want success, got %q", nack)
	}

	// Self-hosted verifier owned by bob but token says alice -> should fail.
	v2 := makeVerifier(t, ks, "bob", "node-1", true, clock)
	nack, _ := v2.VerifyStart(env, fields)
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

	nack, _ := v.VerifyStart(env, fields)
	if nack != NACKCorrespondence {
		t.Fatalf("signed non-empty vs exec empty: want NACKCorrespondence, got %q", nack)
	}
}

// Nil AuthEnvelope in enforced mode must return NACKMissingIntent.
func TestNilEnvelopeEnforcedMode(t *testing.T) {
	_, ks := genASKey(t)
	clock := func() time.Time { return time.Unix(1_770_000_000, 0) }
	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	nack, _ := v.VerifyStart(nil, goodStartFields("sp-1", "node-1", 1))
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
	keyID, _ := token.KeyID(asPriv.Public().(ed25519.PublicKey))
	tokenBody := &authv1.SessionTokenBody{
		AccountId:      "alice",
		TokenId:        "tok-open",
		Audience:       "node",
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(15 * time.Minute).Unix(),
		SessionKeyHash: token.SessionKeyHash(spki),
		KeyId:          keyID,
	}
	nodeTok, _ := token.Mint(tokenBody, asPriv)
	si, _ := intent.Build(intent.OpSessionOpen, body, sessionKey)
	env := &authv1.AuthEnvelope{AccessToken: nodeTok, Intent: si}

	v := makeVerifier(t, ks, "alice", "node-1", false, clock)
	fields := OpenFields{SpawnID: "sp-1", Generation: 1, SessionID: "sess-a", AssertedOwner: "alice"}
	nack, detail := v.VerifyOpen(env, fields)
	if nack != "" {
		t.Fatalf("open happy path: want success, got nack=%s detail=%s", nack, detail)
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
