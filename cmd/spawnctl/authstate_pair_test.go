package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	cpBody, err := parseSessionTokenBody(cp)
	if err != nil {
		t.Fatal(err)
	}
	nodeBody, err := parseSessionTokenBody(node)
	if err != nil {
		t.Fatal(err)
	}
	cpBody.KeyId, nodeBody.KeyId = "", ""
	if _, err := validateCredentialPair(testSessionWire(t, cpBody), testSessionWire(t, nodeBody), key); err == nil {
		t.Fatal("empty signing key id accepted")
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

func TestLogoutKeepsPersistentLockAndClearsBeforeRemote(t *testing.T) {
	dir := t.TempDir()
	requestStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(requestStarted)
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()
	if err := saveState(dir, &authState{ASURL: srv.URL, RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- doLogout(ctx, dir) }()
	<-requestStarted
	if _, err := os.Stat(authStatePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("auth state still exists while remote logout is blocked: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("doLogout: %v", err)
	}
	if _, err := os.Stat(authLockPath(dir)); err != nil {
		t.Fatalf("persistent lock missing after logout: %v", err)
	}
}

func TestLogoutSurfacesLocalRemovalFailureBeforeRemote(t *testing.T) {
	dir := t.TempDir()
	remoteCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { remoteCalls++ }))
	defer srv.Close()
	if err := saveState(dir, &authState{ASURL: srv.URL, RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	previous := removeAuthStateFile
	removeAuthStateFile = func(string) error { return errors.New("remove failed") }
	t.Cleanup(func() { removeAuthStateFile = previous })
	if err := doLogout(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "remove failed") {
		t.Fatalf("doLogout error = %v", err)
	}
	if remoteCalls != 0 {
		t.Fatalf("remote logout called %d times before local clear", remoteCalls)
	}
}

func TestRefreshUnauthorizedSurfacesLocalRemovalFailure(t *testing.T) {
	dir := t.TempDir()
	key, _ := generateSessionKey()
	keyPEM, _ := marshalSessionKey(key)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":"invalid_token"}`)
	}))
	defer srv.Close()
	state := &authState{ASURL: srv.URL, RefreshToken: "refresh", SessionKeyPKCS8PEM: keyPEM}
	previous := removeAuthStateFile
	removeAuthStateFile = func(string) error { return errors.New("remove failed") }
	t.Cleanup(func() { removeAuthStateFile = previous })
	if err := doRefresh(context.Background(), dir, state, srv.Client()); err == nil || !strings.Contains(err.Error(), "remove failed") {
		t.Fatalf("doRefresh error = %v", err)
	}
}

func TestConcurrentRefreshAndLogoutCannotResurrectCredentialsOrSplitLock(t *testing.T) {
	dir := t.TempDir()
	key, _ := generateSessionKey()
	keyPEM, _ := marshalSessionKey(key)
	spkiB64, _ := sessionPubkeySPKIB64(key)
	spki, _ := base64.StdEncoding.DecodeString(spkiB64)
	hash := sha256.Sum256(spki)
	cpToken, nodeToken, _ := testCredentialPair(t, hash[:])
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/refresh":
			close(refreshStarted)
			<-releaseRefresh
			http.SetCookie(w, &http.Cookie{Name: "refresh_token", Value: "refresh-next"})
			_, _ = fmt.Fprintf(w, `{"cp_access_token":%q,"node_access_token":%q}`, cpToken, nodeToken)
		case "/logout":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	if err := saveState(dir, &authState{ASURL: srv.URL, CPAccessToken: cpToken, NodeAccessToken: nodeToken, AccessExpiresAt: time.Now().Unix(), RefreshToken: "refresh-old", SessionKeyPKCS8PEM: keyPEM}); err != nil {
		t.Fatal(err)
	}
	source := &cpTokenSource{dir: dir, httpClient: srv.Client()}
	refreshDone := make(chan error, 1)
	go func() { _, err := source.Token(context.Background()); refreshDone <- err }()
	<-refreshStarted
	before, err := os.Stat(authLockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	logoutDone := make(chan error, 1)
	go func() { logoutDone <- doLogout(context.Background(), dir) }()
	close(releaseRefresh)
	if err := <-refreshDone; err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := <-logoutDone; err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := os.Stat(authStatePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials resurrected after logout: %v", err)
	}
	after, err := os.Stat(authLockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("logout replaced the persistent lock inode")
	}
}
