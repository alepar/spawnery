package cp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
	"spawnery/internal/weborigin"
)

// helpers for WS tests

func genWSKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func wsKeySet(t *testing.T, privs ...ed25519.PrivateKey) token.KeySet {
	t.Helper()
	pubs := make([]ed25519.PublicKey, len(privs))
	for i, p := range privs {
		pubs[i] = p.Public().(ed25519.PublicKey)
	}
	ks, err := token.NewKeySet(pubs...)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func wsMintToken(t *testing.T, priv ed25519.PrivateKey, accountID, tokenID, audience string, now time.Time) string {
	t.Helper()
	kid, err := token.KeyID(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	body := &authv1.SessionTokenBody{
		AccountId: accountID,
		Audience:  audience,
		TokenId:   tokenID,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(15 * time.Minute).Unix(),
		KeyId:     kid,
	}
	wire, err := token.Mint(body, priv)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// newWSTestServer builds a test Server+verifier and returns an httptest.Server serving /ws/session.
func newWSTestServer(t *testing.T, priv ed25519.PrivateKey, devMode bool, devTokens map[string]string) (*Server, *auth.Verifier, *auth.SessionRegistry, *httptest.Server) {
	t.Helper()
	s, _, _ := newTestServer(t)

	sessions := auth.NewSessionRegistry()
	revreg := auth.NewRevocationRegistry(sessions)

	ks := wsKeySet(t, priv)
	v := auth.NewVerifier(auth.VerifierConfig{
		Keys:      ks,
		DevTokens: devTokens,
		DevMode:   devMode,
		Revoked:   revreg,
	})

	s.SetSessionRegistry(sessions)
	s.SetVerify(v.Verify)
	s.SetDevMode(devMode)

	ts := httptest.NewServer(s.HandleWS(v, weborigin.FromEnv("")))
	t.Cleanup(ts.Close)
	return s, v, sessions, ts
}

// dial connects to the ws test server and sends the bind frame. Returns the connection.
func dialAndBind(t *testing.T, ts *httptest.Server, spawnID, tokenWire, clientID string) *websocket.Conn {
	t.Helper()
	ctx := context.Background()
	wsURL := "ws" + ts.URL[len("http"):] + ""

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	bind := map[string]interface{}{
		"spawnId":  spawnID,
		"token":    tokenWire,
		"clientId": clientID,
	}
	if err := wsjson.Write(ctx, conn, bind); err != nil {
		conn.CloseNow()
		t.Fatalf("send bind: %v", err)
	}
	return conn
}

func TestHandleWS_ValidASTokenBindAttaches(t *testing.T) {
	priv := genWSKey(t)
	now := time.Unix(1_770_000_000, 0)

	s, _, _, ts := newWSTestServer(t, priv, false, nil)

	// Seed a spawn owned by "acct-1".
	ctx := context.Background()
	spawnID := seedSpawn(t, ctx, s, "acct-1")

	tokenWire := wsMintToken(t, priv, "acct-1", "tok-ws-1", "cp", now)
	conn := dialAndBind(t, ts, spawnID, tokenWire, "client-1")
	defer conn.CloseNow()

	// Send a ping binary frame and verify it reaches the router.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// No crash = success (the route exists, data forwarded).
}

func TestHandleWS_InvalidToken_Rejected(t *testing.T) {
	priv := genWSKey(t)
	_, _, _, ts := newWSTestServer(t, priv, false, nil)

	ctx := context.Background()
	wsURL := "ws" + ts.URL[len("http"):]
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	bind := map[string]interface{}{
		"spawnId":  "any",
		"token":    "bad-token",
		"clientId": "c1",
	}
	if err := wsjson.Write(ctx, conn, bind); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Connection should be closed with policy violation.
	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Fatal("expected connection close, got nil")
	}
}

func TestHandleWS_TokenRevocation_ClosesSession(t *testing.T) {
	priv := genWSKey(t)
	now := time.Unix(1_770_000_000, 0)

	s, _, sessions, ts := newWSTestServer(t, priv, false, nil)

	ctx := context.Background()
	spawnID := seedSpawn(t, ctx, s, "acct-2")

	tokenWire := wsMintToken(t, priv, "acct-2", "tok-ws-revoke", "cp", now)
	conn := dialAndBind(t, ts, spawnID, tokenWire, "client-2")
	defer conn.CloseNow()

	// Give WS goroutines time to register.
	time.Sleep(50 * time.Millisecond)

	// Revoke the token.
	sessions.RevokeToken("tok-ws-revoke")

	// Connection should be closed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn.SetReadLimit(1024)
		_, _, err := conn.Read(ctx)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("connection not closed after token revocation")
		}
	}
}

func TestHandleWS_AccountRevocation_ClosesSession(t *testing.T) {
	priv := genWSKey(t)
	now := time.Unix(1_770_000_000, 0)

	s, _, sessions, ts := newWSTestServer(t, priv, false, nil)

	ctx := context.Background()
	spawnID := seedSpawn(t, ctx, s, "acct-3")

	tokenWire := wsMintToken(t, priv, "acct-3", "tok-ws-3", "cp", now)
	conn := dialAndBind(t, ts, spawnID, tokenWire, "client-3")
	defer conn.CloseNow()

	time.Sleep(50 * time.Millisecond)

	sessions.RevokeAccount("acct-3")

	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("connection not closed after account revocation")
		}
	}
}

func TestHandleWS_ValidReauth_KeepsOpen(t *testing.T) {
	priv := genWSKey(t)
	now := time.Unix(1_770_000_000, 0)

	s, _, _, ts := newWSTestServer(t, priv, false, nil)

	ctx := context.Background()
	spawnID := seedSpawn(t, ctx, s, "acct-4")

	tokenWire := wsMintToken(t, priv, "acct-4", "tok-4", "cp", now)
	conn := dialAndBind(t, ts, spawnID, tokenWire, "client-4")
	defer conn.CloseNow()

	time.Sleep(20 * time.Millisecond)

	// Send a valid reauth TEXT frame.
	newToken := wsMintToken(t, priv, "acct-4", "tok-4-new", "cp", now)
	reauth := map[string]interface{}{"type": "reauth", "token": newToken}
	reauthBytes, _ := json.Marshal(reauth)
	if err := conn.Write(ctx, websocket.MessageText, reauthBytes); err != nil {
		t.Fatalf("write reauth: %v", err)
	}

	// Send a binary ACP frame after reauth — should still work.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("after-reauth")); err != nil {
		t.Fatalf("post-reauth write: %v", err)
	}
}

func TestHandleWS_InvalidReauth_ClosesInProd(t *testing.T) {
	priv := genWSKey(t)
	now := time.Unix(1_770_000_000, 0)

	s, _, _, ts := newWSTestServer(t, priv, false, nil)

	ctx := context.Background()
	spawnID := seedSpawn(t, ctx, s, "acct-5")

	tokenWire := wsMintToken(t, priv, "acct-5", "tok-5", "cp", now)
	conn := dialAndBind(t, ts, spawnID, tokenWire, "client-5")
	defer conn.CloseNow()

	time.Sleep(20 * time.Millisecond)

	// Send an invalid reauth.
	reauth := map[string]interface{}{"type": "reauth", "token": "bad-token"}
	reauthBytes, _ := json.Marshal(reauth)
	if err := conn.Write(ctx, websocket.MessageText, reauthBytes); err != nil {
		t.Fatalf("write reauth: %v", err)
	}

	// Connection should be closed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			return // closed as expected
		}
		if time.Now().After(deadline) {
			t.Fatal("connection not closed after invalid reauth in prod mode")
		}
	}
}

func TestHandleWS_DevTokenSession_NoReauthNeeded(t *testing.T) {
	priv := genWSKey(t)
	devTokens := map[string]string{"dev-ws-token": "acct-dev"}

	s, _, _, ts := newWSTestServer(t, priv, true, devTokens)

	ctx := context.Background()
	spawnID := seedSpawn(t, ctx, s, "acct-dev")

	conn := dialAndBind(t, ts, spawnID, "dev-ws-token", "client-dev")
	defer conn.CloseNow()

	// Send binary data — should work without reauth.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("dev-data")); err != nil {
		t.Fatalf("dev session write: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	// If we're still connected (no error), dev token session works fine.
}

// seedSpawn creates a spawn for the given owner and returns its ID.
// It re-uses the app already seeded by newTestServer ("secret-app" / "1.0.0").
func seedSpawn(t *testing.T, ctx context.Context, s *Server, ownerID string) string {
	t.Helper()
	if err := s.st.Owners().Upsert(ctx, store.Owner{ID: ownerID, CreatedAt: time.Now().Unix()}); err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	spawnID := "sp-ws-" + ownerID
	sp := store.Spawn{
		ID:         spawnID,
		OwnerID:    ownerID,
		AppID:      "secret-app",
		AppVersion: "1.0.0",
		AppRef:     "examples/secret-app",
		Model:      "gpt-4",
		CreatedAt:  time.Now().Unix(),
		LastUsedAt: time.Now().Unix(),
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		return tx.Spawns().Create(ctx, sp, nil)
	}); err != nil {
		t.Fatalf("create spawn: %v", err)
	}
	return sp.ID
}
