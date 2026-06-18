package authsvc

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

func TestSessionBearerAccount(t *testing.T) {
	now := time.Unix(1770000000, 0)
	fixedNow := func() time.Time { return now }

	// Build a key set.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	kid, err := token.KeyID(pub)
	if err != nil {
		t.Fatalf("keyid: %v", err)
	}
	ks, err := token.NewKeySet(pub)
	if err != nil {
		t.Fatalf("keyset: %v", err)
	}

	mintToken := func(accountID string, issuedAt, expiresAt int64) string {
		wire, e := token.Mint(&authv1.SessionTokenBody{
			KeyId:     kid,
			AccountId: accountID,
			Audience:  "cp",
			IssuedAt:  issuedAt,
			ExpiresAt: expiresAt,
		}, priv)
		if e != nil {
			t.Fatalf("mint: %v", e)
		}
		return wire
	}

	fn := SessionBearerAccount(ks, fixedNow)

	newReq := func(authHeader string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if authHeader != "" {
			r.Header.Set("Authorization", authHeader)
		}
		return r
	}

	t.Run("valid token returns account id", func(t *testing.T) {
		wire := mintToken("acct-7", now.Unix(), now.Add(15*time.Minute).Unix())
		id, ok := fn(newReq("Bearer " + wire))
		if !ok || id != "acct-7" {
			t.Fatalf("got (%q, %v), want (acct-7, true)", id, ok)
		}
	})

	t.Run("expired token returns false", func(t *testing.T) {
		wire := mintToken("acct-7", now.Add(-30*time.Minute).Unix(), now.Add(-1*time.Minute).Unix())
		id, ok := fn(newReq("Bearer " + wire))
		if ok || id != "" {
			t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
		}
	})

	t.Run("forged signature unknown key returns false", func(t *testing.T) {
		_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
		// Mint a token signed by the OTHER key; our ks only has pub (the first key).
		wire, _ := token.Mint(&authv1.SessionTokenBody{
			KeyId:     kid,
			AccountId: "acct-7",
			Audience:  "cp",
			IssuedAt:  now.Unix(),
			ExpiresAt: now.Add(15 * time.Minute).Unix(),
		}, otherPriv)
		id, ok := fn(newReq("Bearer " + wire))
		if ok || id != "" {
			t.Fatalf("got (%q, %v), want (\"\", false) for forged sig", id, ok)
		}
	})

	t.Run("no authorization header returns false", func(t *testing.T) {
		id, ok := fn(newReq(""))
		if ok || id != "" {
			t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
		}
	})

	t.Run("bearer prefix only returns false", func(t *testing.T) {
		id, ok := fn(newReq("Bearer "))
		if ok || id != "" {
			t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
		}
	})
}
