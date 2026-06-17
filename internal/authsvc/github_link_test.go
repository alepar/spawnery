package authsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
)

// drive performs the authorize GET without following the redirect and returns the `code`
// the fake placed in the Location query — local helper so githubfake stays untouched.
func drive(t *testing.T, authURL string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("authorize GET: %v", err)
	}
	defer resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("authorize returned no code: %s", resp.Header.Get("Location"))
	}
	return code
}

func newLinkExchanger(t *testing.T) (GitHubLinkExchanger, *githubfake.Fake) {
	t.Helper()
	f := githubfake.New()
	t.Cleanup(f.Close)
	prov := NewGitHubProvider(f.URL(), f.URL(), f.ClientID, f.ClientSecret)
	ex, ok := prov.(GitHubLinkExchanger)
	if !ok {
		t.Fatalf("provider does not implement GitHubLinkExchanger")
	}
	return ex, f
}

func headerAccount(header string) AccountFromRequest {
	return func(r *http.Request) (string, bool) {
		v := r.Header.Get(header)
		return v, v != ""
	}
}

func newLinkAS(t *testing.T, st store.Store, ex GitHubLinkExchanger, now func() time.Time) *Service {
	t.Helper()
	root, _ := pki.NewRootCA("R")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	return New(root.Cert, inter,
		WithClock(now),
		WithGitHubLink(GitHubLinkConfig{
			Exchanger:          ex,
			Store:              st,
			AppClientID:        "Iv1.spawneryapp",
			RedirectURI:        "https://as.example.com/github/link/callback",
			PostRedeemRedirect: "https://app.example.com/settings/github",
			DefaultHost:        "github.com",
			AccountFromReq:     headerAccount("X-Test-Account"),
			SPAOrigin:          "https://app.example.com",
		}),
	)
}

// seedFlow seeds both a githubLinkState and a githubLinkFlow directly into the service for tests.
func seedFlow(t *testing.T, s *Service, kind githubLinkClientKind, port int, account, secretID string, now time.Time) (flowID, state, verifier string) {
	t.Helper()
	flowID, state, verifier = "flow-"+account, "state-"+account, "ver-"+account
	s.githubLinkMu.Lock()
	s.githubLinkStates[state] = githubLinkState{
		flowID: flowID, accountID: account, secretID: secretID, host: "github.com",
		verifier: verifier, redirectURI: s.githubLinkRedirectURI,
		clientKind: kind, loopbackPort: port, expiresAt: now.Add(time.Minute),
	}
	s.githubLinkFlows[flowID] = &githubLinkFlow{
		accountID: account, secretID: secretID, host: "github.com",
		clientKind: kind, loopbackPort: port, status: flowIssued, expiresAt: now.Add(time.Minute),
	}
	s.githubLinkMu.Unlock()
	return
}

// runCallback drives the fake authorize and calls serveGitHubLinkCallback.
func runCallback(t *testing.T, s *Service, ex GitHubLinkExchanger, state, verifier string) *httptest.ResponseRecorder {
	t.Helper()
	code := drive(t, ex.AppAuthorizeURL(state, pkceChallenge(verifier), s.githubLinkRedirectURI))
	rec := httptest.NewRecorder()
	s.serveGitHubLinkCallback(rec, httptest.NewRequest(http.MethodGet, "/github/link/callback?state="+state+"&code="+code, nil))
	return rec
}

// redeemJSON posts to serveGitHubLinkRedeem with the given fields.
func redeemJSON(s *Service, account, flowID string, confirm bool, rc string, cookie *http.Cookie) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]any{"flow_id": flowID, "confirm_switch": confirm, "rc": rc})
	req := httptest.NewRequest(http.MethodPost, "/github/link/redeem", bytes.NewReader(b))
	if account != "" {
		req.Header.Set("X-Test-Account", account)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.serveGitHubLinkRedeem(rec, req)
	return rec
}

// --- Kept tests ---

func TestExchangeUserTokenReturnsFullTuple(t *testing.T) {
	ex, f := newLinkExchanger(t)
	f.SetUser(424242, "octolink")
	verifier := "verifier-abc"
	redirectURI := "https://as.example.com/github/link/callback"
	authURL := ex.AppAuthorizeURL("state-1", pkceChallenge(verifier), redirectURI)
	code := drive(t, authURL)

	tok, err := ex.ExchangeUserToken(context.Background(), code, verifier, redirectURI)
	if err != nil {
		t.Fatalf("ExchangeUserToken: %v", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" || tok.AccessExpiresAtUnix == 0 || tok.RefreshExpiresAtUnix == 0 {
		t.Fatalf("incomplete tuple: %+v", tok)
	}
	user, err := ex.FetchUser(context.Background(), tok.AccessToken)
	if err != nil {
		t.Fatalf("FetchUser: %v", err)
	}
	if user.Sub != 424242 || user.Login != "octolink" {
		t.Fatalf("user = %+v", user)
	}
}

func TestRevokeAppGrantKillsChainAtGitHub(t *testing.T) {
	ex, f := newLinkExchanger(t)
	// Bootstrap a live grant via the link exchanger.
	verifier := "v-revoke"
	redirectURI := "https://as.example.com/github/link/callback"
	code := drive(t, ex.AppAuthorizeURL("st", pkceChallenge(verifier), redirectURI))
	tok, err := ex.ExchangeUserToken(context.Background(), code, verifier, redirectURI)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if _, err := ex.FetchUser(context.Background(), tok.AccessToken); err != nil {
		t.Fatalf("precondition FetchUser: %v", err)
	}
	if err := ex.RevokeAppGrant(context.Background(), tok.AccessToken); err != nil {
		t.Fatalf("RevokeAppGrant: %v", err)
	}
	// Chain is dead: the access token no longer authenticates.
	if _, err := ex.FetchUser(context.Background(), tok.AccessToken); err == nil {
		t.Fatalf("FetchUser succeeded after RevokeAppGrant; want failure")
	}
	// Idempotent: revoking an already-dead token is a no-op success (GitHub 404 → nil).
	if err := ex.RevokeAppGrant(context.Background(), tok.AccessToken); err != nil {
		t.Fatalf("second RevokeAppGrant should be idempotent, got %v", err)
	}
	_ = f
}

// --- Task 3: CORS unit tests ---

func TestGHLinkCORSCredentialedHeaders(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	noop := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	handler := s.ghLinkCORSCredentialed(noop)

	// OPTIONS preflight from the allowed SPA origin -> ACAC:true + exact ACAO.
	req := httptest.NewRequest(http.MethodOptions, "/github/link/redeem", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("ACAC = %q, want true", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("ACAO = %q, want https://app.example.com", got)
	}

	// POST from the allowed origin -> ACAC:true.
	req2 := httptest.NewRequest(http.MethodPost, "/github/link/redeem", nil)
	req2.Header.Set("Origin", "https://app.example.com")
	rec2 := httptest.NewRecorder()
	handler(rec2, req2)
	if got := rec2.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("POST ACAC = %q, want true", got)
	}

	// Foreign origin -> 403.
	req3 := httptest.NewRequest(http.MethodPost, "/github/link/redeem", nil)
	req3.Header.Set("Origin", "https://evil.example.com")
	rec3 := httptest.NewRecorder()
	handler(rec3, req3)
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("foreign origin status = %d, want 403", rec3.Code)
	}
}

// --- Task 4: serveGitHubLinkStart ---

func TestGitHubLinkStartDerivesSecretIDAndIgnoresClientID(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	b, _ := json.Marshal(map[string]any{"client_kind": "web", "secret_id": "gh:someone-else"})
	req := httptest.NewRequest(http.MethodPost, "/github/link/start", bytes.NewReader(b))
	req.Header.Set("X-Test-Account", "acct-7")
	rec := httptest.NewRecorder()
	s.serveGitHubLinkStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if out["authorize_url"] == "" || out["flow_id"] == "" {
		t.Fatalf("missing authorize_url or flow_id: %v", out)
	}
	s.githubLinkMu.Lock()
	fl := s.githubLinkFlows[out["flow_id"]]
	s.githubLinkMu.Unlock()
	if fl == nil {
		t.Fatalf("flow not created for flow_id %q", out["flow_id"])
	}
	if fl.secretID != "gh:acct-7" {
		t.Fatalf("secretID = %q, want gh:acct-7 (client-supplied ignored)", fl.secretID)
	}
	if fl.accountID != "acct-7" {
		t.Fatalf("accountID = %q", fl.accountID)
	}
	if fl.status != flowIssued {
		t.Fatalf("flow status = %v, want flowIssued", fl.status)
	}
}

func TestGitHubLinkStartRequiresAuth(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	b, _ := json.Marshal(map[string]any{"client_kind": "web"})
	req := httptest.NewRequest(http.MethodPost, "/github/link/start", bytes.NewReader(b))
	// No X-Test-Account header.
	rec := httptest.NewRecorder()
	s.serveGitHubLinkStart(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated start = %d, want 401", rec.Code)
	}
}

func TestGitHubLinkStartLoopbackPortRange(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	cases := []struct {
		body   map[string]any
		wantOK bool
	}{
		{map[string]any{"client_kind": "loopback", "port": 80}, false},
		{map[string]any{"client_kind": "loopback"}, false},
		{map[string]any{"client_kind": "loopback", "port": 51000}, true},
		{map[string]any{"client_kind": "bogus"}, false},
	}
	for _, c := range cases {
		b, _ := json.Marshal(c.body)
		req := httptest.NewRequest(http.MethodPost, "/github/link/start", bytes.NewReader(b))
		req.Header.Set("X-Test-Account", "acct-7")
		rec := httptest.NewRecorder()
		s.serveGitHubLinkStart(rec, req)
		got := rec.Code == http.StatusOK
		if got != c.wantOK {
			t.Errorf("body=%v: status=%d wantOK=%v", c.body, rec.Code, c.wantOK)
		}
	}
}

// --- Task 5: serveGitHubLinkCallback ---

func TestCallbackWebSetsCompleterCookieAndReady(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(990011, "octolink")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, state, verifier := seedFlow(t, s, clientKindWeb, 0, "acct-7", "gh:acct-7", now)
	rec := runCallback(t, s, ex, state, verifier)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, s.githubLinkPostRedeem) {
		t.Fatalf("callback redirect = %q, want prefix %q", loc, s.githubLinkPostRedeem)
	}
	if strings.Contains(loc, "as_gh_completer") {
		t.Fatalf("completer cookie leaked into redirect URL: %q", loc)
	}
	var completerCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == githubLinkCookieName {
			completerCookie = c
		}
	}
	if completerCookie == nil || completerCookie.Value == "" {
		t.Fatalf("no %s cookie set", githubLinkCookieName)
	}
	if !completerCookie.HttpOnly || !completerCookie.Secure || completerCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie security flags wrong: %+v", completerCookie)
	}

	s.githubLinkMu.Lock()
	fl := s.githubLinkFlows[flowID]
	s.githubLinkMu.Unlock()
	if fl.status != flowReady {
		t.Fatalf("flow status = %v, want flowReady", fl.status)
	}
	if fl.completerSecret != completerCookie.Value {
		t.Fatalf("flow completerSecret != cookie.Value")
	}
	if fl.pending == nil || fl.pending.login != "octolink" {
		t.Fatalf("flow pending: %+v", fl.pending)
	}
}

func TestCallbackLoopbackRedirectsWithRC(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(1, "user1")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, state, verifier := seedFlow(t, s, clientKindLoopback, 51000, "acct-7", "gh:acct-7", now)
	rec := runCallback(t, s, ex, state, verifier)
	if rec.Code != http.StatusFound {
		t.Fatalf("loopback callback status = %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://127.0.0.1:51000/done?rc=") {
		t.Fatalf("loopback redirect = %q", loc)
	}
	rcEncoded := strings.TrimPrefix(loc, "http://127.0.0.1:51000/done?rc=")
	rc, _ := url.QueryUnescape(rcEncoded)
	if rc == "" {
		t.Fatalf("rc param empty in %q", loc)
	}
	s.githubLinkMu.Lock()
	fl := s.githubLinkFlows[flowID]
	s.githubLinkMu.Unlock()
	if fl.completerSecret != rc {
		t.Fatalf("flow completerSecret %q != rc %q", fl.completerSecret, rc)
	}
}

func TestCallbackDeviceNoCompleterAndReady(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(2, "user2")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, state, verifier := seedFlow(t, s, clientKindDevice, 0, "acct-7", "gh:acct-7", now)
	rec := runCallback(t, s, ex, state, verifier)
	if rec.Code != http.StatusOK {
		t.Fatalf("device callback status = %d", rec.Code)
	}
	s.githubLinkMu.Lock()
	fl := s.githubLinkFlows[flowID]
	s.githubLinkMu.Unlock()
	if fl.status != flowReady {
		t.Fatalf("flow status = %v, want flowReady", fl.status)
	}
	if fl.completerSecret != "" {
		t.Fatalf("device flow must have no completerSecret, got %q", fl.completerSecret)
	}
}

func TestCallbackUserDenialIsTerminalError(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, state, _ := seedFlow(t, s, clientKindWeb, 0, "acct-7", "gh:acct-7", now)
	rec := httptest.NewRecorder()
	s.serveGitHubLinkCallback(rec, httptest.NewRequest(http.MethodGet, "/github/link/callback?state="+state+"&error=access_denied", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("denial callback status = %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "error=access_denied") {
		t.Fatalf("denial redirect missing error param: %q", loc)
	}
	s.githubLinkMu.Lock()
	fl := s.githubLinkFlows[flowID]
	s.githubLinkMu.Unlock()
	if fl.status != flowError || fl.errorCode != "access_denied" {
		t.Fatalf("flow status/errorCode = %v/%q, want flowError/access_denied", fl.status, fl.errorCode)
	}
}

func TestCallbackJunkCodeIsNoOp(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	_, state, _ := seedFlow(t, s, clientKindWeb, 0, "acct-7", "gh:acct-7", now)
	rec := httptest.NewRecorder()
	s.serveGitHubLinkCallback(rec, httptest.NewRequest(http.MethodGet, "/github/link/callback?state="+state+"&code=totally-bogus", nil))

	// Exchange fails (junk code) -> NO-OP: flow stays ISSUED, state key survives.
	s.githubLinkMu.Lock()
	_, stateExists := s.githubLinkStates[state]
	fl := s.githubLinkFlows["flow-acct-7"]
	s.githubLinkMu.Unlock()
	if !stateExists {
		t.Fatalf("state key must survive after junk code")
	}
	if fl == nil || fl.status != flowIssued {
		t.Fatalf("flow status after junk code: %v", fl)
	}
}

// --- Task 6: serveGitHubLinkRedeem ---

func TestRedeemWebMetadataOnlyNoTokenMaterial(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(990011, "octolink")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, state, verifier := seedFlow(t, s, clientKindWeb, 0, "acct-7", "gh:acct-7", now)
	cbRec := runCallback(t, s, ex, state, verifier)
	var cookie *http.Cookie
	for _, c := range cbRec.Result().Cookies() {
		if c.Name == githubLinkCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("no completer cookie set by callback")
	}

	rdRec := redeemJSON(s, "acct-7", flowID, false, "", cookie)
	if rdRec.Code != http.StatusOK {
		t.Fatalf("redeem status = %d, body=%s", rdRec.Code, rdRec.Body.String())
	}
	body := rdRec.Body.String()
	for _, want := range []string{`"secret_id":"gh:acct-7"`, `"login":"octolink"`, `"version":1`, `"status":"linked"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("redeem body missing %q: %s", want, body)
		}
	}
	for _, bad := range []string{"refresh_token", "access_token", "ghr_", "ghu_"} {
		if strings.Contains(body, bad) {
			t.Fatalf("redeem body MUST NOT contain %q (invariant a): %s", bad, body)
		}
	}
	// Durable row exists.
	link, err := st.GitHubLinks().Get(t.Context(), "gh:acct-7")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if link.Version != 1 {
		t.Fatalf("persisted version = %d, want 1", link.Version)
	}
}

func TestRedeemChannelRuleWebRequiresCookie(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(1, "u")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, state, verifier := seedFlow(t, s, clientKindWeb, 0, "acct-7", "gh:acct-7", now)
	runCallback(t, s, ex, state, verifier)

	// flow_id-only (no cookie) -> 403.
	if got := redeemJSON(s, "acct-7", flowID, false, "", nil).Code; got != http.StatusForbidden {
		t.Fatalf("web flow_id-only = %d, want 403", got)
	}
}

func TestRedeemChannelRuleLoopback(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(1, "u")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, state, verifier := seedFlow(t, s, clientKindLoopback, 51000, "acct-7", "gh:acct-7", now)
	cbRec := runCallback(t, s, ex, state, verifier)

	// flow_id-only (no rc) -> 403.
	if got := redeemJSON(s, "acct-7", flowID, false, "", nil).Code; got != http.StatusForbidden {
		t.Fatalf("loopback flow_id-only = %d, want 403", got)
	}

	// Correct rc extracted from the callback Location -> 200.
	loc := cbRec.Header().Get("Location")
	rcEncoded := strings.TrimPrefix(loc, "http://127.0.0.1:51000/done?rc=")
	rc, _ := url.QueryUnescape(rcEncoded)

	// Need to reseed because the 403 attempt consumed nothing (flow still READY).
	if got := redeemJSON(s, "acct-7", flowID, false, rc, nil).Code; got != http.StatusOK {
		t.Fatalf("loopback with rc = %d, want 200", got)
	}
}

func TestRedeemDeviceAcceptsFlowIDOnly(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(1, "u")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, state, verifier := seedFlow(t, s, clientKindDevice, 0, "acct-7", "gh:acct-7", now)
	runCallback(t, s, ex, state, verifier)

	if got := redeemJSON(s, "acct-7", flowID, false, "", nil).Code; got != http.StatusOK {
		t.Fatalf("device flow_id-only = %d, want 200", got)
	}
}

func TestRedeemIssuedReturns202(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, _, _ := seedFlow(t, s, clientKindDevice, 0, "acct-7", "gh:acct-7", now)
	// No callback -> flow is ISSUED.
	rec := redeemJSON(s, "acct-7", flowID, false, "", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ISSUED flow = %d, want 202", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"pending"`) {
		t.Fatalf("body missing pending status: %s", rec.Body.String())
	}
}

func TestRedeemUnknownFlowReturns404(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	if got := redeemJSON(s, "acct-7", "no-such-flow", false, "", nil).Code; got != http.StatusNotFound {
		t.Fatalf("unknown flow = %d, want 404", got)
	}
}

func TestRedeemErrorFlowReturns400(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, _, _ := seedFlow(t, s, clientKindDevice, 0, "acct-7", "gh:acct-7", now)
	// Force ERROR status.
	s.githubLinkMu.Lock()
	s.githubLinkFlows[flowID].status = flowError
	s.githubLinkFlows[flowID].errorCode = "access_denied"
	s.githubLinkMu.Unlock()

	if got := redeemJSON(s, "acct-7", flowID, false, "", nil).Code; got != http.StatusBadRequest {
		t.Fatalf("error flow = %d, want 400", got)
	}
}

func TestRedeemSingleUseReplay404(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(1, "u")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, state, verifier := seedFlow(t, s, clientKindDevice, 0, "acct-7", "gh:acct-7", now)
	runCallback(t, s, ex, state, verifier)

	if got := redeemJSON(s, "acct-7", flowID, false, "", nil).Code; got != http.StatusOK {
		t.Fatalf("first redeem = %d, want 200", got)
	}
	if got := redeemJSON(s, "acct-7", flowID, false, "", nil).Code; got != http.StatusNotFound {
		t.Fatalf("replay redeem = %d, want 404", got)
	}
}

func TestRedeemExpiredFlow404(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	flowID, _, _ := seedFlow(t, s, clientKindDevice, 0, "acct-7", "gh:acct-7", now)
	// Force expiry.
	s.githubLinkMu.Lock()
	s.githubLinkFlows[flowID].expiresAt = now.Add(-time.Minute)
	s.githubLinkMu.Unlock()

	if got := redeemJSON(s, "acct-7", flowID, false, "", nil).Code; got != http.StatusNotFound {
		t.Fatalf("expired flow = %d, want 404", got)
	}
}

func TestRedeemPeekBeforePopIdentityChange(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	// Link v1 as user 111 @alice.
	f.SetUser(111, "alice")
	flowID1, state1, ver1 := seedFlow(t, s, clientKindDevice, 0, "acct-7", "gh:acct-7", now)
	runCallback(t, s, ex, state1, ver1)
	if got := redeemJSON(s, "acct-7", flowID1, false, "", nil).Code; got != http.StatusOK {
		t.Fatalf("first link = %d", got)
	}

	// Second OAuth as user 222 @bob.
	f.SetUser(222, "bob")
	flowID2, state2, ver2 := seedFlow(t, s, clientKindDevice, 0, "acct-7-b", "gh:acct-7", now)
	// Fix state: use "acct-7" as the flow's accountID (seedFlow uses the account param).
	s.githubLinkMu.Lock()
	s.githubLinkFlows[flowID2].accountID = "acct-7"
	s.githubLinkStates[state2] = githubLinkState{
		flowID: flowID2, accountID: "acct-7", secretID: "gh:acct-7", host: "github.com",
		verifier: ver2, redirectURI: s.githubLinkRedirectURI, clientKind: clientKindDevice,
		expiresAt: now.Add(time.Minute),
	}
	s.githubLinkMu.Unlock()
	runCallback(t, s, ex, state2, ver2)

	// Without confirm_switch -> 409 identity_change, flow stays READY.
	rec409 := redeemJSON(s, "acct-7", flowID2, false, "", nil)
	if rec409.Code != http.StatusConflict {
		t.Fatalf("identity change without confirm = %d, want 409", rec409.Code)
	}
	body409 := rec409.Body.String()
	if !strings.Contains(body409, "alice") || !strings.Contains(body409, "bob") {
		t.Fatalf("409 body must contain old and new login: %s", body409)
	}
	// Flow is still READY (not consumed).
	s.githubLinkMu.Lock()
	fl2 := s.githubLinkFlows[flowID2]
	s.githubLinkMu.Unlock()
	if fl2 == nil || fl2.consumed {
		t.Fatalf("flow must remain ready after 409, consumed=%v", fl2)
	}

	// With confirm_switch -> 200 and new identity committed.
	if got := redeemJSON(s, "acct-7", flowID2, true, "", nil).Code; got != http.StatusOK {
		t.Fatalf("confirm switch = %d, want 200", got)
	}
	link, err := st.GitHubLinks().Get(t.Context(), "gh:acct-7")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if link.GithubUserID != "222" || link.Version != 2 {
		t.Fatalf("after switch: GithubUserID=%s Version=%d", link.GithubUserID, link.Version)
	}
}

func TestRedeemOwnershipGuard(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(1, "u")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	// Pre-seed a row owned by acct-OTHER.
	if err := st.GitHubLinks().Upsert(t.Context(), store.GitHubLink{
		SecretID: "gh:acct-7", AccountID: "acct-OTHER", Host: "github.com", Login: "other",
		GithubUserID: "999", AppClientID: "Iv1.spawneryapp", RefreshToken: "ghr_x",
		AccessToken: "ghu_x", TokenType: "bearer", Version: 1,
		DeliveryID: "github-access-gh:acct-7-v1", UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	flowID, state, verifier := seedFlow(t, s, clientKindDevice, 0, "acct-7", "gh:acct-7", now)
	runCallback(t, s, ex, state, verifier)

	if got := redeemJSON(s, "acct-7", flowID, true, "", nil).Code; got != http.StatusForbidden {
		t.Fatalf("cross-account ownership guard = %d, want 403", got)
	}
}

// --- Task 7: serveGitHubLinkList ---

func TestGitHubLinksListEndpoint(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	// Upsert a link for acct-7 then mark it relink_required.
	if err := st.GitHubLinks().Upsert(t.Context(), store.GitHubLink{
		SecretID: "gh:acct-7", AccountID: "acct-7", Host: "github.com", Login: "octo",
		GithubUserID: "42", AppClientID: "Iv1.spawneryapp", RefreshToken: "ghr_x",
		AccessToken: "ghu_x", TokenType: "bearer", Version: 3,
		DeliveryID: "github-access-gh:acct-7-v3", UpdatedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.GitHubLinks().MarkRelinkRequired(t.Context(), "gh:acct-7", now.Unix()); err != nil {
		t.Fatalf("mark relink: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/github/links", nil)
	req.Header.Set("X-Test-Account", "acct-7")
	rec := httptest.NewRecorder()
	s.serveGitHubLinkList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"status":"relink_required"`, `"secret_id":"gh:acct-7"`, `"login":"octo"`, `"version":3`} {
		if !strings.Contains(body, want) {
			t.Fatalf("list body missing %q: %s", want, body)
		}
	}
	for _, bad := range []string{"refresh_token", "access_token"} {
		if strings.Contains(body, bad) {
			t.Fatalf("list body MUST NOT contain %q: %s", bad, body)
		}
	}

	// A different account sees no links.
	req2 := httptest.NewRequest(http.MethodGet, "/github/links", nil)
	req2.Header.Set("X-Test-Account", "acct-other")
	rec2 := httptest.NewRecorder()
	s.serveGitHubLinkList(rec2, req2)
	if !strings.Contains(rec2.Body.String(), `"links":[]`) {
		t.Fatalf("other account list not empty: %s", rec2.Body.String())
	}
}

// --- Task 8: reaper ---

func TestReaperEvictsExpiredAndRevokesAbandonedAccess(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(1, "u")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	// Seed a READY web flow.
	flowID, state, verifier := seedFlow(t, s, clientKindWeb, 0, "acct-7", "gh:acct-7", now)
	runCallback(t, s, ex, state, verifier)

	// Capture the access token for post-reap verification.
	s.githubLinkMu.Lock()
	fl := s.githubLinkFlows[flowID]
	accessTok := fl.pending.tuple.AccessToken
	fl.expiresAt = now.Add(-time.Minute) // force expiry
	s.githubLinkMu.Unlock()

	// Precondition: access token is live.
	if _, err := ex.FetchUser(context.Background(), accessTok); err != nil {
		t.Fatalf("precondition FetchUser: %v", err)
	}

	// Reap synchronously.
	s.reapGitHubLinkFlows(now)

	// Flow is gone.
	s.githubLinkMu.Lock()
	_, stillThere := s.githubLinkFlows[flowID]
	s.githubLinkMu.Unlock()
	if stillThere {
		t.Fatalf("reaper did not evict the expired flow")
	}

	// Abandoned access token was targeted with DELETE /token: it's now dead.
	if _, err := ex.FetchUser(context.Background(), accessTok); err == nil {
		t.Fatalf("FetchUser after reap should fail (DELETE /token fired), but succeeded")
	}
}

func TestReaperAlsoSweepsExpiredStates(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	// Seed an expired state.
	s.githubLinkMu.Lock()
	s.githubLinkStates["stale"] = githubLinkState{expiresAt: now.Add(-time.Minute)}
	s.githubLinkMu.Unlock()

	s.reapGitHubLinkFlows(now)

	s.githubLinkMu.Lock()
	_, ok := s.githubLinkStates["stale"]
	s.githubLinkMu.Unlock()
	if ok {
		t.Fatalf("reaper did not evict the stale state")
	}
}

// --- Task 9: handler cross-origin live round-trip ---

func TestHandlerRedeemCrossOriginCredentialedRoundTrip(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(990011, "octolink")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	// Stand up a real HTTP server.
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Seed a READY web flow.
	flowID, state, verifier := seedFlow(t, s, clientKindWeb, 0, "acct-7", "gh:acct-7", now)
	cbRec := runCallback(t, s, ex, state, verifier)
	var cookie *http.Cookie
	for _, c := range cbRec.Result().Cookies() {
		if c.Name == githubLinkCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("no completer cookie from callback")
	}

	// POST /github/link/redeem with Origin + cookie (credentials:'include' simulation).
	b, _ := json.Marshal(map[string]any{"flow_id": flowID, "confirm_switch": false, "rc": ""})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/github/link/redeem", bytes.NewReader(b))
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("X-Test-Account", "acct-7")
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /github/link/redeem: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := json.Marshal(nil)
		_ = b
		bodyBytes := make([]byte, 2048)
		n, _ := resp.Body.Read(bodyBytes)
		t.Fatalf("redeem status = %d, body=%s", resp.StatusCode, string(bodyBytes[:n]))
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("ACAO = %q, want https://app.example.com", got)
	}
}
