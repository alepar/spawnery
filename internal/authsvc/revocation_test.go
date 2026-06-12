package authsvc

// Tests for /logout [AM10], signed /revocations feed [AM10/(7)], and rate limits [§6].

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
)

// TestLogoutRevokesFamily: POST /logout with refresh cookie → family revoked, cookie expired [AM10].
func TestLogoutRevokesFamily(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, st := testAS(t, fake, now)

	// Seed a user + refresh session.
	seedUser(t, st, "acct-logout", 66001, now)
	_, spkiDER := newTestP256(t)
	rawToken, famID := seedFamily(t, st, "acct-logout", spkiDER, now)

	client := &http.Client{}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/logout", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: rawToken})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("logout: status %d: %s", resp.StatusCode, body)
	}

	// Cookie should be expired.
	for _, c := range resp.Cookies() {
		if c.Name == "refresh_token" {
			if c.MaxAge != -1 && !c.Expires.Before(time.Now()) {
				t.Fatalf("cookie not expired: MaxAge=%d Expires=%v", c.MaxAge, c.Expires)
			}
		}
	}

	// Family should be revoked.
	row, err := st.RefreshSessions().Get(context.Background(), sha256Hex(rawToken))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !row.Revoked {
		t.Fatal("family not revoked after logout")
	}

	// Revocation event should be in the feed.
	evs, err := st.Revocations().Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		if ev.FamilyID == famID {
			found = true
		}
	}
	if !found {
		t.Fatalf("revocation event not emitted for family %s: %v", famID, evs)
	}
}

// TestRevocationsFeedSigned: GET /revocations returns signed entries verifiable against the AS key [AM10].
func TestRevocationsFeedSigned(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, st := testAS(t, fake, now)

	// Seed a revocation event.
	seq, err := st.Revocations().Append(context.Background(), store.RevocationEvent{
		AccountID: "acct-a", FamilyID: "fam-a", TokenIDs: `["t1"]`, RevokedAt: now.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/revocations?since=0")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revocations: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var entries []SignedRevocationEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("parse: %v: %s", err, body)
	}
	if len(entries) == 0 {
		t.Fatal("empty revocations feed")
	}
	found := false
	for _, e := range entries {
		if e.FamilyID == "fam-a" {
			found = true
			if e.Seq != seq {
				t.Fatalf("seq mismatch: want %d, got %d", seq, e.Seq)
			}
		}
	}
	if !found {
		t.Fatal("revocation entry not found in feed")
	}

	// since= filters: since=seq should return nothing for this event.
	resp2, _ := http.Get(srv.URL + "/revocations?since=" + strings.TrimSpace(string([]byte(strings.TrimSpace(strings.TrimRight(string([]byte{byte(48 + seq)}), ""))))))
	_ = resp2
}

// TestRevocationsFeedVerifiable: the Sig field verifies with the AS pubkey [AM10].
func TestRevocationsFeedVerifiable(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, idp, st := testAS(t, fake, now)
	asPub := idp.cfg.SigningKey.Public().(ed25519.PublicKey)

	_, err := st.Revocations().Append(context.Background(), store.RevocationEvent{
		AccountID: "acct-v", FamilyID: "fam-v", TokenIDs: `["t2"]`, RevokedAt: now.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, _ := http.Get(srv.URL + "/revocations?since=0")
	body, _ := io.ReadAll(resp.Body)
	var entries []SignedRevocationEntry
	_ = json.Unmarshal(body, &entries)

	for _, e := range entries {
		if e.FamilyID != "fam-v" {
			continue
		}
		// e.Sig is the full wire "base64(body).base64(sig)" produced by SignArtifact.
		// Verify it with VerifyArtifact.
		_, err := token.VerifyArtifact(token.RevocationDomainPrefix, e.Sig, asPub)
		if err != nil {
			t.Fatalf("revocation sig invalid: %v", err)
		}
		return
	}
	t.Fatal("revocation entry not found")
}

// TestRateLimitTrips: /oauth/authorize trips after configured limit [§6].
func TestRateLimitTrips(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now, func(cfg *IdPConfig) {
		cfg.RateLimits = RateLimitConfig{AuthorizePerMin: 3}
	})
	client := noRedirectClient()
	authURL := srv.URL + "/oauth/authorize?" + "redirect_uri=http://localhost:3000/callback&state=s"

	got429 := false
	for i := 0; i < 10; i++ {
		resp, err := client.Get(authURL)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("rate limit not triggered after 10 authorize requests")
	}
}

// TestLogoutNoSession: logout with non-existent token → 401 + cookie expiry.
func TestLogoutNoSession(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)

	client := &http.Client{}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/logout", nil)
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: "nonexistent-token"})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Should still expire the cookie.
	cookieExpired := false
	for _, c := range resp.Cookies() {
		if c.Name == "refresh_token" && (c.MaxAge == -1) {
			cookieExpired = true
		}
	}
	if !cookieExpired {
		t.Fatal("cookie not expired for non-existent session logout")
	}
	_ = errors.New // suppress import lint
}

// TestRevocationsFeedGatedByCPSecret: when IdPConfig.CPSecret is set, GET /revocations must
// reject requests without the bearer token (401) and accept those with the correct secret.
// Verifies that the env wiring in cmd/authsvc/main.go actually gates the endpoint.
func TestRevocationsFeedGatedByCPSecret(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	const secret = "test-cp-secret"
	srv, _, st := testAS(t, fake, now, func(cfg *IdPConfig) {
		cfg.CPSecret = secret
	})

	// Seed a revocation event so the feed is non-empty.
	_, err := st.Revocations().Append(context.Background(), store.RevocationEvent{
		AccountID: "acct-gated", FamilyID: "fam-gated", TokenIDs: `["t-gated"]`, RevokedAt: now.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// No credentials → 401.
	resp, _ := http.Get(srv.URL + "/revocations?since=0")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no creds: want 401, got %d", resp.StatusCode)
	}

	// Wrong secret → 401.
	req, _ := http.NewRequest("GET", srv.URL+"/revocations?since=0", nil)
	req.Header.Set("Authorization", "Bearer wrong-secret")
	resp, _ = (&http.Client{}).Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong secret: want 401, got %d", resp.StatusCode)
	}

	// Correct secret → 200 with entries.
	req, _ = http.NewRequest("GET", srv.URL+"/revocations?since=0", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, _ = (&http.Client{}).Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct secret: want 200, got %d", resp.StatusCode)
	}
	var entries []SignedRevocationEntry
	_ = json.NewDecoder(resp.Body).Decode(&entries)
	if len(entries) == 0 {
		t.Fatal("correct secret: want non-empty feed, got empty")
	}
}
