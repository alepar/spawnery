package authsvc

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

func TestSessionBearerAccount(t *testing.T) {
	now := time.Unix(1770000000, 0)
	fixedNow := func() time.Time { return now }

	pki := newTestArtifactPKI(t, now, "prod")
	signer := pki.signer(t, now, "account")
	verifier, err := token.NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	mintToken := func(accountID string, issuedAt, expiresAt int64) string {
		body, e := proto.Marshal(&authv1.SessionTokenBody{
			KeyId:     hex.EncodeToString(signer.KeyID[:]),
			AccountId: accountID,
			Audience:  "cp",
			IssuedAt:  issuedAt,
			ExpiresAt: expiresAt,
		})
		if e != nil {
			t.Fatalf("marshal: %v", e)
		}
		wire, e := signer.Sign(token.ArtifactTypeSession, body)
		if e != nil {
			t.Fatalf("mint: %v", e)
		}
		return wire
	}

	fn := SessionBearerAccount(verifier, fixedNow)

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
		otherPKI := newTestArtifactPKI(t, now, "prod")
		otherSigner := otherPKI.signer(t, now, "other")
		payload, _ := proto.Marshal(&authv1.SessionTokenBody{
			KeyId:     hex.EncodeToString(otherSigner.KeyID[:]),
			AccountId: "acct-7",
			Audience:  "cp",
			IssuedAt:  now.Unix(),
			ExpiresAt: now.Add(15 * time.Minute).Unix(),
		})
		wire, _ := otherSigner.Sign(token.ArtifactTypeSession, payload)
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
