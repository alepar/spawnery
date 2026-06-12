package authsvc

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
)

// testAS builds a Service+httptest.Server for identity flow tests without a real PKI.
// The GitHubRedirectURI is overridden to point at the test server's /oauth/callback.
func testAS(t *testing.T, fake *githubfake.Fake, now time.Time, opts ...func(*IdPConfig)) (*httptest.Server, *IdP, store.Store) {
	t.Helper()

	// We need the srv URL before building the IdP, so use a late-binding approach:
	// start the server first with a placeholder, then patch cfg.
	var idpPtr *IdP
	var storePtr store.Store
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	callbackURI := srv.URL + "/oauth/callback"
	idpOpts := append([]func(*IdPConfig){
		func(cfg *IdPConfig) { cfg.GitHubRedirectURI = callbackURI },
	}, opts...)
	idp, st, _ := newTestIdP(t, fake, now, idpOpts...)
	idpPtr = idp
	storePtr = st

	// Wire routes (idpPtr set above).
	mux.HandleFunc("GET /oauth/authorize", func(w http.ResponseWriter, r *http.Request) { idpPtr.serveAuthorize(w, r) })
	mux.HandleFunc("GET /oauth/callback", func(w http.ResponseWriter, r *http.Request) { idpPtr.serveCallback(w, r) })
	mux.HandleFunc("POST /refresh", idpPtr.corsCredentialed(func(w http.ResponseWriter, r *http.Request) { idpPtr.serveRefresh(w, r) }))
	mux.HandleFunc("OPTIONS /refresh", idpPtr.corsCredentialed(func(w http.ResponseWriter, r *http.Request) {}))
	mux.HandleFunc("POST /logout", idpPtr.corsCredentialed(func(w http.ResponseWriter, r *http.Request) { idpPtr.serveLogout(w, r) }))
	mux.HandleFunc("OPTIONS /logout", idpPtr.corsCredentialed(func(w http.ResponseWriter, r *http.Request) {}))
	mux.HandleFunc("GET /revocations", func(w http.ResponseWriter, r *http.Request) { idpPtr.serveRevocations(w, r) })
	mux.HandleFunc("POST /device/authorize", func(w http.ResponseWriter, r *http.Request) { idpPtr.serveDeviceAuthorize(w, r) })
	mux.HandleFunc("GET /device/verify", func(w http.ResponseWriter, r *http.Request) { idpPtr.serveDeviceVerifyGet(w, r) })
	mux.HandleFunc("POST /device/verify", func(w http.ResponseWriter, r *http.Request) { idpPtr.serveDeviceVerifyPost(w, r) })
	mux.HandleFunc("POST /device/token", func(w http.ResponseWriter, r *http.Request) { idpPtr.serveDeviceToken(w, r) })

	return srv, idp, storePtr
}

// doGet follows at most one redirect but does NOT follow redirects automatically.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// triggerCallback drives the authorize→callback flow with the fake GitHub.
// The SPA redirect_uri goes to the AS callback URL automatically (configured at testAS time).
// Returns the callback response (after GitHub redirect).
func triggerCallback(t *testing.T, srv *httptest.Server, fake *githubfake.Fake) *http.Response {
	t.Helper()
	_, spkiDER := newTestP256(t)
	return triggerCallbackWith(t, srv, fake, "http://localhost:3000/callback", "client-state-abc",
		base64.StdEncoding.EncodeToString(spkiDER))
}

// triggerCallbackWith drives the authorize→callback flow. sessionPubkeyB64 is the base64-encoded
// DER SPKI of the SPA session key; it is required (callback rejects login without it).
func triggerCallbackWith(t *testing.T, srv *httptest.Server, fake *githubfake.Fake, clientRedirectURI, clientState, sessionPubkeyB64 string) *http.Response {
	t.Helper()
	client := noRedirectClient()

	// 1. GET /oauth/authorize: SPA sends redirect_uri + session_pubkey (its own SPA callback route).
	authURL := srv.URL + "/oauth/authorize?" + url.Values{
		"redirect_uri":   {clientRedirectURI},
		"state":          {clientState},
		"session_pubkey": {sessionPubkeyB64},
	}.Encode()

	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: expected 302, got %d (body: check location)", resp.StatusCode)
	}

	// Extract location (AS→GitHub redirect).
	ghURL := resp.Header.Get("Location")
	if ghURL == "" {
		t.Fatal("authorize: no Location header")
	}
	// The flow cookie was set on the authorize response.
	flowCookie := ""
	for _, c := range resp.Cookies() {
		if c.Name == flowCookieName {
			flowCookie = c.Value
		}
	}
	if flowCookie == "" {
		t.Fatal("authorize: no flow cookie")
	}

	// 2. Follow to fake GitHub — which redirects back to the AS /oauth/callback.
	ghResp, err := client.Get(ghURL)
	if err != nil {
		t.Fatalf("fake github authorize: %v", err)
	}
	callbackURL := ghResp.Header.Get("Location")
	if callbackURL == "" {
		t.Fatalf("fake github: no callback Location, status=%d", ghResp.StatusCode)
	}

	// 3. GET /oauth/callback WITH the flow cookie (simulating same browser session).
	cbReq, _ := http.NewRequest("GET", callbackURL, nil)
	cbReq.AddCookie(&http.Cookie{Name: flowCookieName, Value: flowCookie})
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	return cbResp
}

// TestOAuthHappyPath: new sub → user created → cookie + access token.
func TestOAuthHappyPath(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	fake.SetUser(42001, "alice")
	now := time.Unix(1770000000, 0)
	srv, _, st := testAS(t, fake, now)

	resp := triggerCallback(t, srv, fake)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback: want 302, got %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "http://localhost:3000/callback") {
		t.Fatalf("callback: unexpected redirect to %q", location)
	}
	// Should carry access_token + original state.
	if extractQueryParam(location, "access_token") == "" {
		t.Fatalf("callback: no access_token in %q", location)
	}
	if extractQueryParam(location, "state") != "client-state-abc" {
		t.Fatalf("callback: state not echoed in %q", location)
	}
	// Refresh cookie should be set.
	hasCookie := false
	for _, c := range resp.Cookies() {
		if c.Name == "refresh_token" {
			hasCookie = true
			if c.HttpOnly != true || c.Path != "/refresh" {
				t.Fatalf("cookie attrs: %+v", c)
			}
		}
	}
	if !hasCookie {
		t.Fatal("callback: no refresh_token cookie")
	}
	// User should be in the store.
	u, err := st.Users().GetBySub(context.Background(), 42001)
	if err != nil || u.Handle != "alice" {
		t.Fatalf("user not created: %v %+v", err, u)
	}
}

// TestOAuthRegistrationClosed: unknown sub + REGISTRATION_ENABLED=false → structured error redirect.
func TestOAuthRegistrationClosed(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	fake.SetUser(99002, "bob")
	now := time.Unix(1770000000, 0)
	srv, _, st := testAS(t, fake, now, func(cfg *IdPConfig) {
		cfg.RegistrationEnabled = false
	})

	resp := triggerCallback(t, srv, fake)
	location := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("want 302, got %d", resp.StatusCode)
	}
	if extractQueryParam(location, "error") != "registration_closed" {
		t.Fatalf("want registration_closed error, got %q", location)
	}
	// No user created.
	if _, err := st.Users().GetBySub(context.Background(), 99002); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user should not exist: %v", err)
	}
}

// TestOAuthForgedCallback: missing flow cookie → rejected [AM8].
func TestOAuthForgedCallback(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	fake.SetUser(33001, "carol")
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)
	client := noRedirectClient()

	// Authorize to get a valid state.
	authResp, err := client.Get(srv.URL + "/oauth/authorize?" + url.Values{
		"redirect_uri": {"http://localhost:3000/callback"},
		"state":        {"s1"},
	}.Encode())
	if err != nil || authResp == nil {
		t.Fatalf("authorize: %v", err)
	}
	ghURL := authResp.Header.Get("Location")
	ghResp, err := client.Get(ghURL)
	if err != nil || ghResp == nil {
		t.Fatalf("fake github: %v", err)
	}
	callbackURL := ghResp.Header.Get("Location")
	if callbackURL == "" {
		t.Fatal("no callback URL from fake github")
	}

	// Callback WITHOUT the flow cookie (forged/injected).
	cbReq, _ := http.NewRequest("GET", callbackURL, nil)
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatal(err)
	}
	// Should redirect with error (not success).
	location := cbResp.Header.Get("Location")
	if cbResp.StatusCode == http.StatusFound && extractQueryParam(location, "access_token") != "" {
		t.Fatal("forged callback succeeded with access_token")
	}
}

// TestOAuthCodeInjection: valid state but wrong flow cookie → rejected [AM8].
func TestOAuthCodeInjection(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	fake.SetUser(44001, "dave")
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)
	client := noRedirectClient()

	// Authorize to get valid state.
	authResp, err := client.Get(srv.URL + "/oauth/authorize?" + url.Values{
		"redirect_uri": {"http://localhost:3000/callback"},
		"state":        {"s2"},
	}.Encode())
	if err != nil || authResp == nil {
		t.Fatalf("authorize: %v", err)
	}
	ghURL := authResp.Header.Get("Location")
	ghResp, err := client.Get(ghURL)
	if err != nil || ghResp == nil {
		t.Fatalf("fake github: %v", err)
	}
	callbackURL := ghResp.Header.Get("Location")
	if callbackURL == "" {
		t.Fatal("no callback URL from fake github")
	}

	// Callback with a DIFFERENT (wrong) flow cookie — state bound to victim's session.
	cbReq, _ := http.NewRequest("GET", callbackURL, nil)
	cbReq.AddCookie(&http.Cookie{Name: flowCookieName, Value: "attacker-flow-id"})
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatal(err)
	}
	location := cbResp.Header.Get("Location")
	if cbResp.StatusCode == http.StatusFound && extractQueryParam(location, "access_token") != "" {
		t.Fatal("code injection succeeded with wrong flow cookie")
	}
}

// TestOAuthRedirectURIExact: unregistered redirect_uri → rejected; loopback port relaxation [AM8].
func TestOAuthRedirectURIExact(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	// Register a loopback redirect URI so port relaxation has a path anchor.
	srv, _, _ := testAS(t, fake, now, func(cfg *IdPConfig) {
		cfg.RedirectURIs = append(cfg.RedirectURIs, "http://127.0.0.1:8000/cb")
	})
	client := noRedirectClient()

	// Unregistered non-loopback redirect_uri → 400.
	resp, _ := client.Get(srv.URL + "/oauth/authorize?" + url.Values{
		"redirect_uri": {"https://evil.example.com/steal"},
		"state":        {"s3"},
	}.Encode())
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unregistered URI: want 400, got %d", resp.StatusCode)
	}

	// Loopback URI with registered path at a DIFFERENT port is allowed (RFC 8252 §7.3 port relaxation).
	resp2, _ := client.Get(srv.URL + "/oauth/authorize?" + url.Values{
		"redirect_uri": {"http://127.0.0.1:12345/cb"},
		"state":        {"s4"},
	}.Encode())
	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("loopback port relaxation: want 302, got %d", resp2.StatusCode)
	}

	// Loopback URI with an unregistered path → 400 (path is not relaxed, only port).
	resp3, _ := client.Get(srv.URL + "/oauth/authorize?" + url.Values{
		"redirect_uri": {"http://127.0.0.1:12345/unregistered-path"},
		"state":        {"s5"},
	}.Encode())
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("loopback unregistered path: want 400, got %d", resp3.StatusCode)
	}
}

// TestOAuthAccessDenied: provider returns access_denied → structured error redirect.
func TestOAuthAccessDenied(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	fake.DenyNext()
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)
	client := noRedirectClient()

	authResp, _ := client.Get(srv.URL + "/oauth/authorize?" + url.Values{
		"redirect_uri": {"http://localhost:3000/callback"},
		"state":        {"s5"},
	}.Encode())
	flowCookie := ""
	for _, c := range authResp.Cookies() {
		if c.Name == flowCookieName {
			flowCookie = c.Value
		}
	}
	ghURL := authResp.Header.Get("Location")

	// Follow to fake GitHub (returns access_denied).
	ghResp, _ := client.Get(ghURL)
	callbackURL := ghResp.Header.Get("Location")

	cbReq, _ := http.NewRequest("GET", callbackURL, nil)
	cbReq.AddCookie(&http.Cookie{Name: flowCookieName, Value: flowCookie})
	cbResp, _ := client.Do(cbReq)
	location := cbResp.Header.Get("Location")
	if extractQueryParam(location, "error") == "" {
		t.Fatalf("access_denied not propagated to SPA: %q", location)
	}
}

// TestOAuthCallbackNoPubkeyRejected: callback without session_pubkey → invalid_request redirect [Fix 3].
func TestOAuthCallbackNoPubkeyRejected(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	fake.SetUser(55001, "nopubkey")
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)
	client := noRedirectClient()

	// Authorize without session_pubkey.
	authResp, _ := client.Get(srv.URL + "/oauth/authorize?" + url.Values{
		"redirect_uri": {"http://localhost:3000/callback"},
		"state":        {"s-nopubkey"},
	}.Encode())
	flowCookie := ""
	for _, c := range authResp.Cookies() {
		if c.Name == flowCookieName {
			flowCookie = c.Value
		}
	}
	ghURL := authResp.Header.Get("Location")
	ghResp, _ := client.Get(ghURL)
	callbackURL := ghResp.Header.Get("Location")

	cbReq, _ := http.NewRequest("GET", callbackURL, nil)
	cbReq.AddCookie(&http.Cookie{Name: flowCookieName, Value: flowCookie})
	cbResp, _ := client.Do(cbReq)
	location := cbResp.Header.Get("Location")
	if cbResp.StatusCode != http.StatusFound || extractQueryParam(location, "error") != "invalid_request" {
		t.Fatalf("want 302 invalid_request redirect, got %d %q", cbResp.StatusCode, location)
	}
	if extractQueryParam(location, "access_token") != "" {
		t.Fatal("callback without pubkey must not return access_token")
	}
}

// TestOAuthLoopbackDeliversRefreshToken: loopback redirect_uri → refresh_token in query (A3 seam).
// SPA (non-loopback) path must NOT carry refresh_token in query.
func TestOAuthLoopbackDeliversRefreshToken(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	fake.SetUser(70001, "loopback-user")
	now := time.Unix(1770000000, 0)
	// Register a loopback anchor URI so loopback port-relaxation works.
	srv, _, st := testAS(t, fake, now, func(cfg *IdPConfig) {
		cfg.RedirectURIs = append(cfg.RedirectURIs, "http://127.0.0.1:8000/cb")
	})
	_, spkiDER := newTestP256(t)
	pubB64 := spkiB64(spkiDER)

	// 1. Loopback redirect_uri (port differs from registered — RFC 8252 §7.3 port relaxation).
	resp := triggerCallbackWith(t, srv, fake, "http://127.0.0.1:54321/cb", "loop-state", pubB64)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("loopback callback: want 302, got %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	refreshToken := extractQueryParam(location, "refresh_token")
	if refreshToken == "" {
		t.Fatalf("loopback: refresh_token must be in query, got location %q", location)
	}
	accessToken := extractQueryParam(location, "access_token")
	if accessToken == "" {
		t.Fatalf("loopback: access_token missing from location %q", location)
	}
	// ClientKind must be "cli" for the loopback family.
	row, err := st.RefreshSessions().Get(context.Background(), sha256Hex(refreshToken))
	if err != nil {
		t.Fatalf("get refresh session: %v", err)
	}
	if row.ClientKind != store.ClientCLI {
		t.Fatalf("loopback: want client_kind=cli, got %q", row.ClientKind)
	}

	// 2. SPA (non-loopback) redirect_uri: refresh_token must NOT appear in the query.
	fake.SetUser(70002, "spa-user")
	resp2 := triggerCallback(t, srv, fake) // uses "http://localhost:3000/callback"
	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("spa callback: want 302, got %d", resp2.StatusCode)
	}
	loc2 := resp2.Header.Get("Location")
	if extractQueryParam(loc2, "refresh_token") != "" {
		t.Fatalf("SPA callback must NOT include refresh_token in query, got %q", loc2)
	}
}

// TestRefreshPerAccountRateLimit: per-account rate limit on /refresh fires for a second request
// from the same account [§6, Fix 1].
func TestRefreshPerAccountRateLimit(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, st := testAS(t, fake, now, func(cfg *IdPConfig) {
		cfg.RateLimits = RateLimitConfig{RefreshPerMin: 1}
	})
	client := noRedirectClient()

	// Seed a user with two refresh families (different tokens, same account).
	seedUser(t, st, "acct-rl2", 88002, now)
	_, spkiDER1 := newTestP256(t)
	_, spkiDER2 := newTestP256(t)
	token1, _ := seedFamily(t, st, "acct-rl2", spkiDER1, now)
	token2, _ := seedFamily(t, st, "acct-rl2", spkiDER2, now)

	// First request: per-account rate limit allows (burst=1), but PoP is absent → 401.
	req1, _ := http.NewRequest("POST", srv.URL+"/refresh", nil)
	req1.AddCookie(&http.Cookie{Name: "refresh_token", Value: token1})
	req1.Header.Set("Origin", "http://localhost:3000")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("refresh req1: %v", err)
	}
	if resp1.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("first request must not be rate-limited (burst=1): got 429")
	}
	if resp1.StatusCode != http.StatusUnauthorized {
		t.Logf("req1 status=%d (expected 401 pop_required)", resp1.StatusCode)
	}

	// Second request (different token, same account): per-account limit exhausted → 429.
	req2, _ := http.NewRequest("POST", srv.URL+"/refresh", nil)
	req2.AddCookie(&http.Cookie{Name: "refresh_token", Value: token2})
	req2.Header.Set("Origin", "http://localhost:3000")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("refresh req2: %v", err)
	}
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request from same account must be rate-limited (429), got %d", resp2.StatusCode)
	}
}
