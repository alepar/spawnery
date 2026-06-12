package cp

// devmint_test.go: cross-component test that the dev AS key path in SubmitIntent mints a
// token that the node's IntentVerifier accepts end-to-end [AM12].
//
// This is the "AM12 cross-component vector": it exercises the full chain from a
// client calling SubmitIntent (with empty NodeAccessToken) through the CP's dev-AS token
// minting, to the node's NewIntentVerifier accepting the resulting envelope.
//
// The test does NOT require a running node or any network connections. It wires the CP and
// node packages together in-process using their production code paths and test-injectable
// seams.

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
	"spawnery/internal/intent"
	"spawnery/internal/node"
)

// TestDevModeMintsVerifiableToken verifies the full AM12 chain:
//  1. The CP mints a cnf-bearing aud=node token from the intent's SPKI DER in SubmitIntent
//     when the client omits NodeAccessToken and a dev AS key is configured.
//  2. The minted token passes token.Verify with the dev AS pubkey.
//  3. The resulting AuthEnvelope passes node.NewIntentVerifier.VerifyStart in enforced mode,
//     proving the boot-generated dev keypair drives the full A4 verification chain.
func TestDevModeMintsVerifiableToken(t *testing.T) {
	s, _, _ := newTestServer(t)

	// 1. Generate a dev AS Ed25519 keypair and configure it on the server (mirrors cp/main.go).
	asPub, asPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := token.KeyID(asPub)
	if err != nil {
		t.Fatal(err)
	}
	s.SetDevASKey(asPriv, keyID)
	// Fix the server's clock so token expiry is deterministic.
	fixedNow := time.Unix(1_770_000_000, 0)
	s.now = func() time.Time { return fixedNow }

	// 2. Seed a spawn in the store (owner = "alice").
	ctx := auth.WithOwner(context.Background(), "alice")
	spawnID := "sp-devmint"
	sp := store.Spawn{
		ID: spawnID, OwnerID: "alice", AppID: "secret-app", AppVersion: "1.0.0",
		AppRef: "examples/secret-app", Model: "m", Image: "",
		Status: store.Starting, CreatedAt: fixedNow.Unix(), LastUsedAt: fixedNow.Unix(),
	}
	if err := s.st.WithTx(context.Background(), func(tx store.Store) error {
		return tx.Spawns().Create(context.Background(), sp, nil)
	}); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}

	// 3. Register a pending intent so SubmitIntent has an entry to submit to.
	pi := &cpv1.PendingIntent{
		SpawnId:    spawnID,
		Generation: 1,
		Op:         string(intent.OpCreateSpawn),
	}
	ch := s.pendingIntents.register(spawnID, "alice", pi)

	// 4. Build a real ephemeral session key + signed intent (mirrors pollAndSign in spawnctl).
	sessionKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := &authv1.IntentBody{
		Jti:        "jti-devmint-test",
		IssuedAt:   fixedNow.Unix(),
		SpawnId:    spawnID,
		Generation: 1,
		Op:         string(intent.OpCreateSpawn),
	}
	si, err := intent.Build(intent.OpCreateSpawn, body, sessionKey)
	if err != nil {
		t.Fatalf("intent.Build: %v", err)
	}

	// 5. Call SubmitIntent with no NodeAccessToken; the CP should mint one from devASKey.
	_, err = s.SubmitIntent(ctx, connect.NewRequest(&cpv1.SubmitIntentRequest{
		SpawnId:         spawnID,
		Intent:          si,
		NodeAccessToken: "", // empty → CP mints in dev mode
	}))
	if err != nil {
		t.Fatalf("SubmitIntent: %v", err)
	}

	// 6. Read the AuthEnvelope the CP submitted to the pending channel.
	var env *authv1.AuthEnvelope
	select {
	case env = <-ch:
	default:
		t.Fatal("no AuthEnvelope delivered to pending channel after SubmitIntent")
	}
	if env == nil {
		t.Fatal("delivered envelope is nil")
	}
	if env.GetAccessToken() == "" {
		t.Fatal("AuthEnvelope.AccessToken is empty — dev AS minting did not run")
	}
	if env.GetIntent() == nil {
		t.Fatal("AuthEnvelope.Intent is nil")
	}

	// 7. Verify the token with the dev AS pubkey: it must have aud=node and the correct cnf.
	ks, err := token.NewKeySet(asPub)
	if err != nil {
		t.Fatalf("NewKeySet: %v", err)
	}
	tokenBody, err := token.Verify(env.GetAccessToken(), ks, fixedNow)
	if err != nil {
		t.Fatalf("token.Verify: %v (dev AS key must produce a verifiable token)", err)
	}
	if tokenBody.Audience != "node" {
		t.Fatalf("token aud=%q want node", tokenBody.Audience)
	}
	if !intent.SPKIMatchesHash(si.SpkiDer, tokenBody.SessionKeyHash) {
		t.Fatal("token.session_key_hash does not match SPKI SHA-256 — cnf binding broken")
	}
	if tokenBody.AccountId != "alice" {
		t.Fatalf("token account_id=%q want alice", tokenBody.AccountId)
	}

	// 8. Feed the full AuthEnvelope into node.NewIntentVerifier and assert it passes in
	//    enforced mode. This is the AM12 cross-component proof: the same env the CP minted
	//    is accepted by the node verifier that would run at container-start time.
	verifier := node.NewIntentVerifier(ks, "", "", false, node.AuthModeEnforced, func() time.Time { return fixedNow })
	fields := node.StartFields{
		SpawnID:       spawnID,
		Generation:    1,
		AssertedOwner: "alice",
	}
	if nack, detail := verifier.VerifyStart(env, fields); nack != "" {
		t.Fatalf("node.VerifyStart rejected the dev-minted envelope: nack=%s detail=%s", nack, detail)
	}
}
