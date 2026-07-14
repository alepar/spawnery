package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/intent"
	"spawnery/internal/runtime/fakepod"
	"spawnery/internal/spawnlet"
)

func signedNodeEnvelope(t *testing.T, signer testArtifactSigner, sessionKey *ecdsa.PrivateKey, accountID, tokenID string, expiresAt time.Time, body *authv1.IntentBody, op intent.Op) *authv1.AuthEnvelope {
	t.Helper()
	spki, err := x509.MarshalPKIXPublicKey(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	tokenBody := &authv1.SessionTokenBody{
		AccountId: accountID, TokenId: tokenID, Audience: token.AudienceNode,
		IssuedAt: body.GetIssuedAt(), ExpiresAt: expiresAt.Unix(), SessionKeyHash: token.SessionKeyHash(spki),
		KeyId: hex.EncodeToString(signer.KeyID[:]),
	}
	payload, err := proto.Marshal(tokenBody)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := signer.Sign(token.ArtifactTypeSession, payload)
	if err != nil {
		t.Fatal(err)
	}
	signedIntent, err := intent.Build(op, body, sessionKey)
	if err != nil {
		t.Fatal(err)
	}
	return &authv1.AuthEnvelope{AccessToken: wire, Intent: signedIntent}
}

func openEnvelope(t *testing.T, signer testArtifactSigner, key *ecdsa.PrivateKey, now time.Time, account, tokenID, jti, spawnID, sessionID string, generation uint64) *authv1.AuthEnvelope {
	t.Helper()
	return signedNodeEnvelope(t, signer, key, account, tokenID, now.Add(time.Hour), &authv1.IntentBody{
		Jti: jti, IssuedAt: now.Unix(), Op: string(intent.OpSessionOpen), SpawnId: spawnID,
		Generation: generation, SessionId: sessionID, TargetNodeId: "node-1",
	}, intent.OpSessionOpen)
}

func reauthEnvelope(t *testing.T, signer testArtifactSigner, key *ecdsa.PrivateKey, now time.Time, account, tokenID, jti, spawnID, sessionID string, generation uint64) *authv1.AuthEnvelope {
	t.Helper()
	return signedNodeEnvelope(t, signer, key, account, tokenID, now.Add(time.Hour), &authv1.IntentBody{
		Jti: jti, IssuedAt: now.Unix(), Op: string(intent.OpSessionReauth), SpawnId: spawnID,
		Generation: generation, SessionId: sessionID, TargetNodeId: "node-1", NewTokenId: tokenID,
	}, intent.OpSessionReauth)
}

func authClosedForAttachment(stream *fakeCPStream, attachmentID string) *nodev1.SessionAuthClosed {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	for _, message := range stream.sent {
		if closed := message.GetSessionAuthClosed(); closed != nil && closed.GetAttachmentId() == attachmentID {
			return closed
		}
	}
	return nil
}

func authClosedSummary(stream *fakeCPStream) string {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	var values []string
	for _, message := range stream.sent {
		if closed := message.GetSessionAuthClosed(); closed != nil {
			values = append(values, closed.GetAttachmentId()+":"+closed.GetReason())
		}
	}
	return strings.Join(values, "; ")
}

func currentSessionToken(registry *sessionAuthRegistry, key sessionAuthKey) string {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if current := registry.records[key]; current != nil {
		return current.record.tokenID
	}
	return ""
}

func TestUserRevocationLifecycleClosesCurrentIncarnationsAndRecoversAfterRestart(t *testing.T) {
	now := time.Unix(1_770_000_000, 0)
	signer, artifacts := genASKey(t)
	statePath := t.TempDir() + "/user-revocations/revocations.db"
	store, err := OpenUserRevocationStore(statePath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	appDir := writeNodeApp(t)
	mgr := newGooseManager(t, fakeBackend(t, fakepod.WithAttachScript(scriptGoose)))
	if _, err := mgr.CreateAuthorizedWithSelection(t.Context(), "sp-live", appDir, "m", "", "", 1, "alice", spawnlet.AgentSelection{}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.CreateAuthorizedWithSelection(t.Context(), "sp-bob", appDir, "m", "", "", 1, "bob", spawnlet.AgentSelection{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Stop(context.Background(), "sp-live") })
	t.Cleanup(func() { _ = mgr.Stop(context.Background(), "sp-bob") })
	stream := &fakeCPStream{}
	a := newAttacher(mgr, stream)
	a.cfg.NodeID = "node-1"
	a.auths = newSessionAuthRegistryWithClock(func() time.Time { return now }, func(delay time.Duration, callback func()) sessionAuthTimer { return time.AfterFunc(delay, callback) }, store)
	a.verifier = NewIntentVerifier(artifacts, "", "node-1", false, func() time.Time { return now }, store)

	directSession := sessionAuthKey{spawnID: "sp-live", sessionID: "direct", clientID: "direct-client"}
	relaySession := sessionAuthKey{spawnID: "sp-live", sessionID: "relay", clientID: "relay-client"}
	bobSession := sessionAuthKey{spawnID: "sp-bob", sessionID: "bob", clientID: "bob-client"}
	directPump := newPump(io.Discard, strings.NewReader(""))
	bobPump := newPump(io.Discard, strings.NewReader(""))
	relay := newTmuxRelay([]string{"cat"}, func(string, []byte) error { return nil })
	t.Cleanup(func() { relay.stop() })
	a.pumps[sessionKey{spawnID: "sp-live", sessionID: "direct"}] = directPump
	a.pumps[sessionKey{spawnID: "sp-bob", sessionID: "bob"}] = bobPump
	a.tmuxRelays[sessionKey{spawnID: "sp-live", sessionID: "relay"}] = relay

	aliceKey := genECDSA(t)
	bobKey := genECDSA(t)
	open := func(spawnID, sessionID, clientID, attachmentID string, sequence uint64, envelope *authv1.AuthEnvelope, owner string) {
		a.handle(t.Context(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{
			SpawnId: spawnID, Generation: 1, SessionId: sessionID, ClientId: clientID, AssertedOwner: owner,
			AttachmentId: attachmentID, AttachmentSequence: sequence, Auth: envelope,
		}}})
	}
	open("sp-live", "direct", "direct-client", "direct-attachment-1", 1, openEnvelope(t, signer, aliceKey, now, "alice", "direct-old", "open-direct-1", "sp-live", "direct", 1), "alice")
	open("sp-live", "relay", "relay-client", "relay-attachment", 1, openEnvelope(t, signer, aliceKey, now, "alice", "relay-old", "open-relay", "sp-live", "relay", 1), "alice")
	open("sp-bob", "bob", "bob-client", "bob-attachment", 1, openEnvelope(t, signer, bobKey, now, "bob", "bob-token", "open-bob", "sp-bob", "bob", 1), "bob")
	if !a.auths.contains(directSession) || !a.auths.contains(relaySession) || !a.auths.contains(bobSession) || !directPump.attached() || !relay.attached() || !bobPump.attached() {
		t.Fatalf("signed opens auth=%v/%v/%v transport=%v/%v/%v closes=%s", a.auths.contains(directSession), a.auths.contains(relaySession), a.auths.contains(bobSession), directPump.attached(), relay.attached(), bobPump.attached(), authClosedSummary(stream))
	}

	a.reauthenticateClient(&nodev1.SessionReauth{
		SpawnId: "sp-live", Generation: 1, SessionId: "direct", ClientId: "direct-client", AssertedOwner: "alice", AttachmentId: "direct-attachment-1",
		Auth: reauthEnvelope(t, signer, aliceKey, now, "alice", "direct-reauth", "reauth-direct", "sp-live", "direct", 1),
	})
	if got := currentSessionToken(a.auths, directSession); got != "direct-reauth" {
		t.Fatalf("reauth token=%q", got)
	}
	open("sp-live", "direct", "direct-client", "direct-attachment-2", 2, openEnvelope(t, signer, aliceKey, now, "alice", "direct-current", "open-direct-2", "sp-live", "direct", 1), "alice")
	if attachment, ok := a.auths.attachment(directSession); !ok || attachment != "direct-attachment-2" {
		t.Fatalf("direct incarnation=%q/%v", attachment, ok)
	}

	familyBatch := []VerifiedUserRevocation{{
		Seq: 2, AccountID: "alice", FamilyID: "family-a", RevokedAt: now.Unix(),
		RevokedTokens: []VerifiedRevokedToken{
			{TokenID: "direct-old", RetainUntil: now.Add(time.Hour).Unix()},
			{TokenID: "direct-reauth", RetainUntil: now.Add(time.Hour).Unix()},
			{TokenID: "relay-old", RetainUntil: now.Add(time.Hour).Unix()},
		},
	}}
	if err := store.ApplyPage(familyBatch, now); err != nil {
		t.Fatal(err)
	}
	a.auths.revoke(familyBatch)
	if !a.auths.contains(directSession) || !directPump.attached() {
		t.Fatal("stale family token closed current direct incarnation")
	}
	if a.auths.contains(relaySession) || relay.attached() {
		t.Fatal("family revocation left relayed attachment live")
	}
	if closed := authClosedForAttachment(stream, "relay-attachment"); closed == nil || closed.GetClientId() != "relay-client" {
		t.Fatalf("relay addressed close=%+v", closed)
	}
	if authClosedForAttachment(stream, "direct-attachment-1") != nil || authClosedForAttachment(stream, "direct-attachment-2") != nil {
		t.Fatal("family revocation closed a nonmatching direct incarnation")
	}

	cutoff := now.Unix()
	accountBatch := []VerifiedUserRevocation{{
		Seq: 5, AccountID: "alice", RevokedAt: cutoff, RevokeTokensIssuedBefore: cutoff,
		RevokedTokens: []VerifiedRevokedToken{{TokenID: "direct-current", RetainUntil: now.Add(time.Hour).Unix()}},
	}}
	if err := store.ApplyPage(accountBatch, now); err != nil {
		t.Fatal(err)
	}
	a.auths.revoke(accountBatch)
	if a.auths.contains(directSession) || directPump.attached() {
		t.Fatal("account revocation left current direct attachment live")
	}
	if !a.auths.contains(bobSession) || !bobPump.attached() {
		t.Fatal("alice account revocation closed unrelated account")
	}
	if closed := authClosedForAttachment(stream, "direct-attachment-2"); closed == nil || closed.GetClientId() != "direct-client" {
		t.Fatalf("direct addressed close=%+v", closed)
	}
	if authClosedForAttachment(stream, "bob-attachment") != nil {
		t.Fatal("unrelated account received addressed close")
	}
	a.detachClient("sp-bob", "bob", "bob-client")

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenUserRevocationStore(statePath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restartStream := &fakeCPStream{}
	restartedAttacher := newAttacher(mgr, restartStream)
	restartedAttacher.cfg.NodeID = "node-1"
	restartedAttacher.auths = newSessionAuthRegistryWithClock(func() time.Time { return now }, func(delay time.Duration, callback func()) sessionAuthTimer { return time.AfterFunc(delay, callback) }, restarted)
	restartedAttacher.verifier = NewIntentVerifier(artifacts, "", "node-1", false, func() time.Time { return now }, restarted)
	restartKey := genECDSA(t)
	blockedStartBody := &authv1.IntentBody{Jti: "restart-blocked", IssuedAt: now.Unix(), Op: string(intent.OpCreateSpawn), SpawnId: "sp-blocked", TargetNodeId: "node-1", AppRef: appDir, Model: "m"}
	restartedAttacher.startSpawn(t.Context(), &nodev1.StartSpawn{
		SpawnId: "sp-blocked", AppRef: appDir, Model: "m", AssertedOwner: "alice", IntentOp: string(intent.OpCreateSpawn),
		Auth: signedNodeEnvelope(t, signer, restartKey, "alice", "direct-current", now.Add(time.Hour), blockedStartBody, intent.OpCreateSpawn),
	})
	if detail := restartStream.lastErrorDetail("sp-blocked"); !strings.Contains(detail, string(NACKTokenInvalid)) {
		t.Fatalf("persisted explicit-token denial=%q", detail)
	}

	freshStartBody := &authv1.IntentBody{Jti: "restart-fresh", IssuedAt: cutoff, Op: string(intent.OpCreateSpawn), SpawnId: "sp-fresh", TargetNodeId: "node-1", AppRef: appDir, Model: "m"}
	restartedAttacher.startSpawn(t.Context(), &nodev1.StartSpawn{
		SpawnId: "sp-fresh", AppRef: appDir, Model: "m", AssertedOwner: "alice", IntentOp: string(intent.OpCreateSpawn),
		Auth: signedNodeEnvelope(t, signer, restartKey, "alice", "fresh-after-account-revoke", now.Add(time.Hour), freshStartBody, intent.OpCreateSpawn),
	})
	if owner, _, ok := mgr.SpawnOwnerGeneration("sp-fresh"); !ok || owner != "alice" {
		t.Fatalf("cutoff-equal start owner=%q ok=%v error=%q", owner, ok, restartStream.lastErrorDetail("sp-fresh"))
	}
	t.Cleanup(func() { _ = mgr.Stop(context.Background(), "sp-fresh") })

	freshPump := newPump(io.Discard, strings.NewReader(""))
	freshKey := sessionAuthKey{spawnID: "sp-live", sessionID: "fresh-open", clientID: "fresh-client"}
	restartedAttacher.pumps[sessionKey{spawnID: "sp-live", sessionID: "fresh-open"}] = freshPump
	freshOpen := &nodev1.SessionOpen{SpawnId: "sp-live", Generation: 1, SessionId: "fresh-open", ClientId: "fresh-client", AssertedOwner: "alice", AttachmentId: "fresh-open-attachment", AttachmentSequence: 1,
		Auth: openEnvelope(t, signer, restartKey, now, "alice", "fresh-open-after-restart", "restart-open", "sp-live", "fresh-open", 1)}
	restartedAttacher.handle(t.Context(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: freshOpen}})
	if !restartedAttacher.auths.contains(freshKey) || !freshPump.attached() || authClosedForAttachment(restartStream, "fresh-open-attachment") != nil {
		t.Fatalf("cutoff-equal open auth=%v attached=%v closes=%s", restartedAttacher.auths.contains(freshKey), freshPump.attached(), authClosedSummary(restartStream))
	}
	restartedAttacher.reauthenticateClient(&nodev1.SessionReauth{
		SpawnId: "sp-live", Generation: 1, SessionId: "fresh-open", ClientId: "fresh-client", AssertedOwner: "alice", AttachmentId: "fresh-open-attachment",
		Auth: reauthEnvelope(t, signer, restartKey, now, "alice", "fresh-reauth-after-restart", "restart-reauth", "sp-live", "fresh-open", 1),
	})
	if got := currentSessionToken(restartedAttacher.auths, freshKey); got != "fresh-reauth-after-restart" {
		t.Fatalf("cutoff-equal reauth token=%q closes=%s", got, authClosedSummary(restartStream))
	}

	now = now.Add(time.Hour)
	if err := restarted.ApplyPage(nil, now); err != nil {
		t.Fatal(err)
	}
	if restarted.Checkpoint() != 5 || restarted.IsRevoked("direct-current", "alice", cutoff) {
		t.Fatalf("pruned state checkpoint=%d revoked=%v", restarted.Checkpoint(), restarted.IsRevoked("direct-current", "alice", cutoff))
	}
}

func TestRevocationOutageCannotExtendOrReauthenticateLiveSignedAttachments(t *testing.T) {
	now := time.Unix(1_770_000_000, 0)
	issuedAt := now
	expiresAt := now.Add(time.Minute)
	signer, artifacts := genASKey(t)
	store, err := OpenUserRevocationStore(t.TempDir()+"/user-revocations/revocations.db", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	appDir := writeNodeApp(t)
	mgr := newGooseManager(t, fakeBackend(t, fakepod.WithAttachScript(scriptGoose)))
	if _, err := mgr.CreateAuthorizedWithSelection(t.Context(), "sp-outage", appDir, "m", "", "", 1, "alice", spawnlet.AgentSelection{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Stop(context.Background(), "sp-outage") })
	stream := &fakeCPStream{}
	a := newAttacher(mgr, stream)
	a.cfg.NodeID = "node-1"
	var timers []*heldTimer
	a.auths = newSessionAuthRegistryWithClock(func() time.Time { return now }, func(_ time.Duration, callback func()) sessionAuthTimer {
		timer := &heldTimer{callback: callback}
		timers = append(timers, timer)
		return timer
	}, store)
	a.verifier = NewIntentVerifier(artifacts, "", "node-1", false, func() time.Time { return now }, store)
	expiryPump := newPump(io.Discard, strings.NewReader(""))
	deniedPump := newPump(io.Discard, strings.NewReader(""))
	a.pumps[sessionKey{spawnID: "sp-outage", sessionID: "expiry"}] = expiryPump
	a.pumps[sessionKey{spawnID: "sp-outage", sessionID: "denied"}] = deniedPump
	key := genECDSA(t)
	openWithExpiry := func(sessionID, clientID, attachmentID, tokenID, jti string, expiry time.Time) {
		body := &authv1.IntentBody{Jti: jti, IssuedAt: issuedAt.Unix(), Op: string(intent.OpSessionOpen), SpawnId: "sp-outage", Generation: 1, SessionId: sessionID, TargetNodeId: "node-1"}
		a.handle(t.Context(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{
			SpawnId: "sp-outage", Generation: 1, SessionId: sessionID, ClientId: clientID, AssertedOwner: "alice", AttachmentId: attachmentID, AttachmentSequence: 1,
			Auth: signedNodeEnvelope(t, signer, key, "alice", tokenID, expiry, body, intent.OpSessionOpen),
		}}})
	}
	openWithExpiry("expiry", "expiry-client", "expiry-attachment", "expiry-token", "outage-open-expiry", expiresAt)
	openWithExpiry("denied", "denied-client", "denied-attachment", "current-token", "outage-open-denied", now.Add(time.Hour))
	if !expiryPump.attached() || !deniedPump.attached() || len(timers) != 2 {
		t.Fatalf("opens attached=%v/%v timers=%d", expiryPump.attached(), deniedPump.attached(), len(timers))
	}

	var polls atomic.Int32
	consumer, err := NewRevocationConsumer(revocationDoer(func(*http.Request) (*http.Response, error) { polls.Add(1); return nil, errors.New("AS unavailable") }), "https://as/revocations", artifacts, store, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	outageCtx, cancelOutage := context.WithCancel(t.Context())
	outageDone := make(chan struct{})
	go func() { consumer.Run(outageCtx, a.auths.revoke); close(outageDone) }()
	waitFor(t, "repeated revocation feed failures", func() bool { return polls.Load() >= 3 })

	if err := store.ApplyPage([]VerifiedUserRevocation{{
		Seq: 1, AccountID: "alice", FamilyID: "family", RevokedAt: issuedAt.Unix(),
		RevokedTokens: []VerifiedRevokedToken{{TokenID: "denied-replacement", RetainUntil: now.Add(time.Hour).Unix()}},
	}}, now); err != nil {
		t.Fatal(err)
	}
	deniedBody := &authv1.IntentBody{Jti: "outage-reauth-denied", IssuedAt: issuedAt.Unix(), Op: string(intent.OpSessionReauth), SpawnId: "sp-outage", Generation: 1, SessionId: "denied", TargetNodeId: "node-1", NewTokenId: "denied-replacement"}
	a.reauthenticateClient(&nodev1.SessionReauth{
		SpawnId: "sp-outage", Generation: 1, SessionId: "denied", ClientId: "denied-client", AssertedOwner: "alice", AttachmentId: "denied-attachment",
		Auth: signedNodeEnvelope(t, signer, key, "alice", "denied-replacement", now.Add(time.Hour), deniedBody, intent.OpSessionReauth),
	})
	if deniedPump.attached() || a.auths.contains(sessionAuthKey{spawnID: "sp-outage", sessionID: "denied", clientID: "denied-client"}) {
		t.Fatal("denied signed reauth left attachment live")
	}
	if closed := authClosedForAttachment(stream, "denied-attachment"); closed == nil || !strings.Contains(closed.GetReason(), string(NACKTokenInvalid)) {
		t.Fatalf("denied reauth close=%+v", closed)
	}
	if len(timers) != 2 {
		t.Fatalf("denied reauth created replacement deadline: timers=%d", len(timers))
	}

	now = expiresAt
	timers[0].callback()
	if expiryPump.attached() || a.auths.contains(sessionAuthKey{spawnID: "sp-outage", sessionID: "expiry", clientID: "expiry-client"}) {
		t.Fatal("original signed expiry did not close live attachment")
	}
	if closed := authClosedForAttachment(stream, "expiry-attachment"); closed == nil || closed.GetReason() != "node authorization expired" {
		t.Fatalf("expiry close=%+v", closed)
	}
	expiredBody := &authv1.IntentBody{Jti: "outage-reauth-expired", IssuedAt: issuedAt.Unix(), Op: string(intent.OpSessionReauth), SpawnId: "sp-outage", Generation: 1, SessionId: "expiry", TargetNodeId: "node-1", NewTokenId: "expired-replacement"}
	a.reauthenticateClient(&nodev1.SessionReauth{
		SpawnId: "sp-outage", Generation: 1, SessionId: "expiry", ClientId: "expiry-client", AssertedOwner: "alice", AttachmentId: "expired-reauth-address",
		Auth: signedNodeEnvelope(t, signer, key, "alice", "expired-replacement", expiresAt, expiredBody, intent.OpSessionReauth),
	})
	if closed := authClosedForAttachment(stream, "expired-reauth-address"); closed == nil || !strings.Contains(closed.GetReason(), string(NACKTokenInvalid)) {
		t.Fatalf("expired reauth close=%+v", closed)
	}
	if len(timers) != 2 {
		t.Fatalf("outage extended deadline: timers=%d", len(timers))
	}
	cancelOutage()
	select {
	case <-outageDone:
	case <-time.After(time.Second):
		t.Fatal("outage consumer did not cancel")
	}
}
