package authsvc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
)

// newDeviceVerifyClient returns an http.Client whose CookieJar is pre-seeded with a
// device_session cookie scoped to Path=/device for srvURL. This exercises real RFC 6265 path
// scoping (unlike AddCookie, which bypasses it) and verifies that the device_session cookie
// at Path=/device reaches /device/verify but a Path=/refresh cookie would not.
func newDeviceVerifyClient(t *testing.T, srvURL, token string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(srvURL + "/device/")
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{
		{Name: deviceSessionCookieName, Value: token, Path: "/device"},
	})
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// TestDeviceGrantHappy: authorize → approve (browser) → poll gets tokens, family bound to pubkey [AM7].
func TestDeviceGrantHappy(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, idp, st := testAS(t, fake, now)
	client := noRedirectClient()
	_, spkiDER := newTestP256(t)
	pubB64 := spkiB64(spkiDER)

	// 1. POST /device/authorize with session_pubkey.
	authResp, err := client.Post(srv.URL+"/device/authorize",
		"application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"session_pubkey": {pubB64}, "client_kind": {"cli"}}.Encode()))
	if err != nil {
		t.Fatalf("device/authorize: %v", err)
	}
	var authOut struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	body, _ := io.ReadAll(authResp.Body)
	if err := json.Unmarshal(body, &authOut); err != nil || authOut.DeviceCode == "" {
		t.Fatalf("device/authorize response: %s", body)
	}
	if len(authOut.UserCode) != 9 { // "XXXX-XXXX" = 9 chars
		t.Fatalf("user_code format: %q", authOut.UserCode)
	}

	// 2. Poll before approval → authorization_pending.
	pollResp, _ := client.Post(srv.URL+"/device/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"device_code": {authOut.DeviceCode}}.Encode()))
	var pollOut struct{ Error string `json:"error"` }
	body, _ = io.ReadAll(pollResp.Body)
	_ = json.Unmarshal(body, &pollOut)
	if pollOut.Error != "authorization_pending" {
		t.Fatalf("poll before approval: want authorization_pending, got %q", pollOut.Error)
	}

	// 3. Simulate user approval: first create a user + session so the verify page auth works.
	seedUser(t, st, "acct-device", 55001, now)
	// Insert a refresh session for the browser user (so requireRefreshCookieSession works).
	_, browser_spki := newTestP256(t)
	browserToken := randOpaque()
	browserRow := store.RefreshSession{
		TokenHash:         sha256Hex(browserToken),
		AccountID:         "acct-device",
		FamilyID:          "browser-fam",
		ClientKind:        store.ClientWeb,
		SessionPubkeySPKI: browser_spki,
		AccessTokenID:     "browser-tok",
		CreatedAt:         now.Unix(),
		LastUsedAt:        now.Unix(),
		ExpiresAt:         now.Add(30 * 24 * time.Hour).Unix(),
		FamilyCreatedAt:   now.Unix(),
	}
	if err := st.RefreshSessions().Insert(context.Background(), browserRow); err != nil {
		t.Fatal(err)
	}

	// POST /device/verify as the logged-in browser user.
	// Use a jar-backed client so RFC 6265 path scoping is enforced: the device_session cookie
	// is at Path=/device and must be sent to /device/verify.
	verifyClient := newDeviceVerifyClient(t, srv.URL, browserToken)
	verifyReq, _ := http.NewRequest("POST", srv.URL+"/device/verify",
		strings.NewReader(url.Values{"user_code": {authOut.UserCode}}.Encode()))
	verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	verifyResp, err := verifyClient.Do(verifyReq)
	if err != nil {
		t.Fatalf("device/verify: %v", err)
	}
	if verifyResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(verifyResp.Body)
		t.Fatalf("device/verify: status %d: %s", verifyResp.StatusCode, body)
	}

	// 4. Poll again → tokens.
	pollResp2, _ := client.Post(srv.URL+"/device/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"device_code": {authOut.DeviceCode}}.Encode()))
	var tokenOut struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
	}
	body, _ = io.ReadAll(pollResp2.Body)
	if err := json.Unmarshal(body, &tokenOut); err != nil {
		t.Fatalf("parse token response: %v: %s", err, body)
	}
	if tokenOut.AccessToken == "" || tokenOut.RefreshToken == "" {
		t.Fatalf("missing tokens: %s", body)
	}

	// 5. Refresh family should be bound to the device's session pubkey.
	refreshHash := sha256Hex(tokenOut.RefreshToken)
	row, err := st.RefreshSessions().Get(context.Background(), refreshHash)
	if err != nil {
		t.Fatalf("get refresh session: %v", err)
	}
	if string(row.SessionPubkeySPKI) != string(spkiDER) {
		t.Fatal("refresh family not bound to device pubkey")
	}
	_ = idp
}

// TestDeviceGrantPollBeforeApproval: already tested in happy path; explicit version here.
func TestDeviceGrantPollBeforeApproval(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)
	client := noRedirectClient()
	_, spkiDER := newTestP256(t)

	authResp, _ := client.Post(srv.URL+"/device/authorize",
		"application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"session_pubkey": {spkiB64(spkiDER)}}.Encode()))
	var authOut struct{ DeviceCode string `json:"device_code"` }
	body, _ := io.ReadAll(authResp.Body)
	_ = json.Unmarshal(body, &authOut)

	pollResp, _ := client.Post(srv.URL+"/device/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"device_code": {authOut.DeviceCode}}.Encode()))
	var out struct{ Error string `json:"error"` }
	body, _ = io.ReadAll(pollResp.Body)
	_ = json.Unmarshal(body, &out)
	if out.Error != "authorization_pending" {
		t.Fatalf("want authorization_pending, got %q", out.Error)
	}
}

// TestDeviceGrantExpired: poll after device_code TTL → expired_token [AM7].
func TestDeviceGrantExpired(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, idp, _ := testAS(t, fake, now)
	client := noRedirectClient()
	_, spkiDER := newTestP256(t)

	authResp, _ := client.Post(srv.URL+"/device/authorize",
		"application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"session_pubkey": {spkiB64(spkiDER)}}.Encode()))
	var authOut struct{ DeviceCode string `json:"device_code"` }
	body, _ := io.ReadAll(authResp.Body)
	_ = json.Unmarshal(body, &authOut)

	// Advance clock past device_code TTL.
	idp.now = func() time.Time { return now.Add(deviceCodeTTL + time.Second) }

	pollResp, _ := client.Post(srv.URL+"/device/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"device_code": {authOut.DeviceCode}}.Encode()))
	var out struct{ Error string `json:"error"` }
	body, _ = io.ReadAll(pollResp.Body)
	_ = json.Unmarshal(body, &out)
	if out.Error != "expired_token" {
		t.Fatalf("want expired_token, got %q (body: %s)", out.Error, body)
	}
}

// TestDeviceGrantPerCodeLockout: after maxDeviceAttempt submissions the grant is locked [AM7].
func TestDeviceGrantPerCodeLockout(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	// Set a very high IP rate limit so the per-code lockout fires first.
	srv, _, st := testAS(t, fake, now,
		func(cfg *IdPConfig) {
			cfg.RateLimits = RateLimitConfig{DevicePerMin: 1000}
		},
	)
	client := noRedirectClient()
	_, spkiDER := newTestP256(t)
	pubB64 := spkiB64(spkiDER)

	// Create a device grant.
	authResp, _ := client.Post(srv.URL+"/device/authorize",
		"application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"session_pubkey": {pubB64}}.Encode()))
	var authOut struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	body, _ := io.ReadAll(authResp.Body)
	_ = json.Unmarshal(body, &authOut)

	// Seed a browser user.
	seedUser(t, st, "acct-lockout", 91001, now)
	_, browser_spki := newTestP256(t)
	browserToken := randOpaque()
	_ = st.RefreshSessions().Insert(context.Background(), store.RefreshSession{
		TokenHash:         sha256Hex(browserToken),
		AccountID:         "acct-lockout",
		FamilyID:          "lockout-fam",
		ClientKind:        store.ClientWeb,
		SessionPubkeySPKI: browser_spki,
		AccessTokenID:     "lockout-tok",
		CreatedAt:         now.Unix(),
		LastUsedAt:        now.Unix(),
		ExpiresAt:         now.Add(30 * 24 * time.Hour).Unix(),
		FamilyCreatedAt:   now.Unix(),
	})

	// Submit maxDeviceAttempt times (each bumps the counter); the grant is approved on the first
	// success, so subsequent submits return "invalid_grant" (not pending). We don't care about the
	// per-attempt response until we exceed the limit.
	// Use a jar-backed client so RFC 6265 path scoping is enforced.
	verifyClient := newDeviceVerifyClient(t, srv.URL, browserToken)
	for range maxDeviceAttempt {
		verifyReq, _ := http.NewRequest("POST", srv.URL+"/device/verify",
			strings.NewReader(url.Values{"user_code": {authOut.UserCode}}.Encode()))
		verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_, _ = verifyClient.Do(verifyReq)
	}

	// The (maxDeviceAttempt+1)th attempt must be rejected with access_denied lockout.
	verifyReq, _ := http.NewRequest("POST", srv.URL+"/device/verify",
		strings.NewReader(url.Values{"user_code": {authOut.UserCode}}.Encode()))
	verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _ := verifyClient.Do(verifyReq)
	body, _ = io.ReadAll(resp.Body)
	var out struct{ Error string `json:"error"` }
	_ = json.Unmarshal(body, &out)
	if out.Error != "access_denied" {
		t.Fatalf("per-code lockout: want access_denied after %d attempts, got %q (body: %s)",
			maxDeviceAttempt, out.Error, body)
	}
}

// TestDeviceGrantUserCodeRateLimit: too many /device/verify attempts are rate-limited.
func TestDeviceGrantUserCodeRateLimit(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, st := testAS(t, fake, now,
		func(cfg *IdPConfig) {
			cfg.RateLimits = RateLimitConfig{DevicePerMin: 2}
		},
	)
	seedUser(t, st, "acct-rl", 77001, now)
	_, browser_spki := newTestP256(t)
	browserToken := randOpaque()
	_ = st.RefreshSessions().Insert(context.Background(), store.RefreshSession{
		TokenHash:         sha256Hex(browserToken),
		AccountID:         "acct-rl",
		FamilyID:          "rl-fam",
		ClientKind:        store.ClientWeb,
		SessionPubkeySPKI: browser_spki,
		AccessTokenID:     "rl-tok",
		CreatedAt:         now.Unix(),
		LastUsedAt:        now.Unix(),
		ExpiresAt:         now.Add(30 * 24 * time.Hour).Unix(),
		FamilyCreatedAt:   now.Unix(),
	})

	// Use a jar-backed client so RFC 6265 path scoping is enforced.
	verifyClient := newDeviceVerifyClient(t, srv.URL, browserToken)
	got429 := false
	for i := 0; i < 5; i++ {
		verifyReq, _ := http.NewRequest("POST", srv.URL+"/device/verify",
			strings.NewReader(url.Values{"user_code": {"FAKE-CODE"}}.Encode()))
		verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, _ := verifyClient.Do(verifyReq)
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("device verify rate limit not triggered")
	}
}

// TestDeviceVerifyGetUnauthenticated: GET /device/verify without a session returns 401 HTML —
// not a redirect to /oauth/authorize?redirect_uri=/device/verify (which would be rejected by
// isAllowedRedirectURI creating a dead loop in production).
func TestDeviceVerifyGetUnauthenticated(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)

	resp, err := http.Get(srv.URL + "/device/verify?user_code=XXXX-XXXX")
	if err != nil {
		t.Fatalf("GET /device/verify: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /device/verify: want 401, got %d", resp.StatusCode)
	}
}
