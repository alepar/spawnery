package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	clientpkg "spawnery/internal/client"
)

func testSessionWire(t *testing.T, body *authv1.SessionTokenBody) string {
	t.Helper()
	payload, err := proto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(&authv1.SignedAuthArtifact{ArtifactType: "session-token", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func testCredentialPair(t *testing.T, keyHash []byte) (string, string, int64) {
	t.Helper()
	now := time.Now().Unix()
	base := authv1.SessionTokenBody{
		AccountId: "acct-1", FamilyId: "fam-1", IssuedAt: now, ExpiresAt: now + 900,
		SessionKeyHash: keyHash, KeyId: "signer-1",
	}
	cp := proto.Clone(&base).(*authv1.SessionTokenBody)
	cp.Audience, cp.TokenId = "cp", "cp-1"
	node := proto.Clone(&base).(*authv1.SessionTokenBody)
	node.Audience, node.TokenId = "node", "node-1"
	return testSessionWire(t, cp), testSessionWire(t, node), base.ExpiresAt
}

func TestValidateCredentialPairRequiresAlignedAudiencesAndKey(t *testing.T) {
	key, err := generateSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	spki, err := sessionPubkeySPKIB64(key)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(spki)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(decoded)
	cp, node, expires := testCredentialPair(t, hash[:])

	gotExpiry, err := validateCredentialPair(cp, node, key)
	if err != nil {
		t.Fatalf("validateCredentialPair: %v", err)
	}
	if gotExpiry != expires {
		t.Fatalf("expiry = %d, want %d", gotExpiry, expires)
	}

	wrongKey, _ := generateSessionKey()
	if _, err := validateCredentialPair(cp, node, wrongKey); err == nil {
		t.Fatal("wrong session key accepted")
	}
	if _, err := validateCredentialPair(cp, cp, key); err == nil {
		t.Fatal("CP token accepted as node token")
	}
}

func TestNodeCredentialsReturnsNodeTokenAndStoredSigner(t *testing.T) {
	dir := t.TempDir()
	key, err := generateSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := marshalSessionKey(key)
	if err != nil {
		t.Fatal(err)
	}
	wireSPKI, err := sessionPubkeySPKIB64(key)
	if err != nil {
		t.Fatal(err)
	}
	rawSPKI, _ := base64.StdEncoding.DecodeString(wireSPKI)
	if _, err := x509.ParsePKIXPublicKey(rawSPKI); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(rawSPKI)
	cp, node, expires := testCredentialPair(t, hash[:])
	if err := saveState(dir, &authState{ASURL: "http://invalid", CPAccessToken: cp, NodeAccessToken: node, AccessExpiresAt: expires, RefreshToken: "refresh", SessionKeyPKCS8PEM: keyPEM}); err != nil {
		t.Fatal(err)
	}

	src := &cpTokenSource{dir: dir}
	credentials, err := src.NodeCredentials(context.Background())
	if err != nil {
		t.Fatalf("NodeCredentials: %v", err)
	}
	if credentials.AccessToken != node {
		t.Fatal("NodeCredentials returned anything other than the node token")
	}
	spkiGot, err := credentials.Signer.PublicSPKIDER()
	if err != nil || sha256.Sum256(spkiGot) != hash {
		t.Fatalf("stored signer mismatch: %v", err)
	}
	var _ clientpkg.NodeCredentialSource = src
}

func TestNodeCredentialsKeyLossClearsStateAndRequiresLogin(t *testing.T) {
	dir := t.TempDir()
	if err := saveState(dir, &authState{ASURL: "", CPAccessToken: "cp", NodeAccessToken: "node", RefreshToken: "refresh", SessionKeyPKCS8PEM: "corrupt"}); err != nil {
		t.Fatal(err)
	}
	src := &cpTokenSource{dir: dir}
	_, err := src.NodeCredentials(context.Background())
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("error = %v, want login required", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, authStateFile)); !os.IsNotExist(statErr) {
		t.Fatalf("auth state still exists: %v", statErr)
	}
}

func TestTokenRefreshKeyLossDoesNotReturnStaleCPToken(t *testing.T) {
	dir := t.TempDir()
	if err := saveState(dir, &authState{ASURL: "", CPAccessToken: "stale-cp", NodeAccessToken: "node", AccessExpiresAt: time.Now().Unix(), RefreshToken: "refresh", SessionKeyPKCS8PEM: "corrupt"}); err != nil {
		t.Fatal(err)
	}
	src := &cpTokenSource{dir: dir}
	token, err := src.Token(context.Background())
	if err == nil || token != "" || !strings.Contains(err.Error(), "login") {
		t.Fatalf("Token = %q, %v; want empty login-required error", token, err)
	}
}
