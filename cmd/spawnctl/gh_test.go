package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
)

// bearerAccount maps the Bearer token straight to an account id, so the spawnctl client's
// `Authorization: Bearer <acct>` authenticates as account <acct> in tests.
var bearerAccount authsvc.AccountFromRequest = func(r *http.Request) (string, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, p) {
		return "", false
	}
	tok := strings.TrimPrefix(h, p)
	return tok, tok != ""
}

// buildLinkAS spins up a real authsvc.Service with the GitHub-link surface wired to githubfake.
// RedirectURI points back at the test AS so githubfake's browser redirect lands on the real
// callback handler.
func buildLinkAS(t *testing.T) (*httptest.Server, *githubfake.Fake, store.Store) {
	t.Helper()
	fake := githubfake.New()
	t.Cleanup(fake.Close)
	ex, ok := authsvc.NewGitHubProvider(fake.URL(), fake.URL(), fake.ClientID, fake.ClientSecret).(authsvc.GitHubLinkExchanger)
	if !ok {
		t.Fatal("provider does not implement GitHubLinkExchanger")
	}
	st := store.NewTestStore(t)
	root, err := pki.NewRootCA("R")
	if err != nil {
		t.Fatal(err)
	}
	inter, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatal(err)
	}
	lm := &lazyTestMux{}
	srv := httptest.NewServer(lm)
	t.Cleanup(srv.Close)
	svc := authsvc.New(root.Cert, inter,
		authsvc.WithGitHubLink(authsvc.GitHubLinkConfig{
			Exchanger:          ex,
			Store:              st,
			AppClientID:        "Iv1.app",
			RedirectURI:        srv.URL + "/github/link/callback",
			PostRedeemRedirect: "https://app.example.com/settings/github",
			DefaultHost:        "github.com",
			AccountFromReq:     bearerAccount,
			SPAOrigin:          "https://app.example.com",
		}),
	)
	lm.real = svc.Handler()
	return srv, fake, st
}

// driveBrowser plays the "browser": GET the authorize URL following every redirect, so githubfake
// issues a code, the AS callback exchanges + fetches the user, and the flow becomes READY (for
// loopback the final hop is 127.0.0.1:<port>/done?rc=). Blocks until the chain settles.
func driveBrowser(t *testing.T, authURL string) {
	t.Helper()
	resp, err := http.Get(authURL) //nolint:bodyclose
	if err != nil {
		t.Fatalf("drive browser GET: %v", err)
	}
	_ = resp.Body.Close()
}

func TestGhCmdRegistration(t *testing.T) {
	c := ghCmd()
	if c.Name != "gh" {
		t.Fatalf("name = %q, want gh", c.Name)
	}
	want := map[string]bool{"link": false, "status": false, "revoke": false}
	for _, sub := range c.Commands {
		if _, ok := want[sub.Name]; ok {
			want[sub.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

func TestDoStartAndDeviceRedeem(t *testing.T) {
	srv, fake, _ := buildLinkAS(t)
	fake.SetUser(424242, "alice")
	hc := &http.Client{Timeout: 5 * time.Second}
	ctx := context.Background()
	bearer := "acct-alice"

	authURL, flowID, err := doStart(ctx, hc, srv.URL, bearer, "device", 0, "")
	if err != nil {
		t.Fatalf("doStart: %v", err)
	}
	if authURL == "" || flowID == "" {
		t.Fatalf("empty authorize_url/flow_id")
	}

	// Before authorization: redeem is pending.
	res, err := doRedeem(ctx, hc, srv.URL, bearer, flowID, "", false)
	if err != nil {
		t.Fatalf("doRedeem pending: %v", err)
	}
	if res.outcome != redeemPending {
		t.Fatalf("outcome = %v, want redeemPending", res.outcome)
	}

	// Drive the browser, then redeem succeeds with metadata only.
	driveBrowser(t, authURL)
	res, err = doRedeem(ctx, hc, srv.URL, bearer, flowID, "", false)
	if err != nil {
		t.Fatalf("doRedeem done: %v", err)
	}
	if res.outcome != redeemDone {
		t.Fatalf("outcome = %v, want redeemDone (%+v)", res.outcome, res)
	}
	if res.link.Login != "alice" || res.link.GithubUserID != "424242" {
		t.Errorf("link = %+v, want login=alice id=424242", res.link)
	}
	if res.link.SecretID != "gh:acct-alice" {
		t.Errorf("secret_id = %q, want gh:acct-alice", res.link.SecretID)
	}
	if res.link.Version != 1 {
		t.Errorf("version = %d, want 1", res.link.Version)
	}
	if res.link.Status != "linked" {
		t.Errorf("status = %q, want linked", res.link.Status)
	}
}

// scriptedRedeem returns a server whose /github/link/redeem replies with scripted statuses and
// whose /github/link/start replies with a fixed flow, so the poll/branch logic is deterministic.
func scriptedRedeem(t *testing.T, statuses []int, bodies []string) *httptest.Server {
	t.Helper()
	var i int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /github/link/start", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authorize_url":"https://example/authorize","flow_id":"flow-1"}`))
	})
	mux.HandleFunc("POST /github/link/redeem", func(w http.ResponseWriter, _ *http.Request) {
		idx := i
		if idx >= len(statuses) {
			idx = len(statuses) - 1
		}
		i++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statuses[idx])
		_, _ = w.Write([]byte(bodies[idx]))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDoRedeemStatusMapping(t *testing.T) {
	hc := &http.Client{Timeout: 2 * time.Second}
	ctx := context.Background()
	cases := []struct {
		status int
		body   string
		want   redeemOutcome
	}{
		{http.StatusAccepted, `{"status":"pending"}`, redeemPending},
		{http.StatusUnauthorized, `{"error":"unauthorized"}`, redeemUnauthorized},
		{http.StatusConflict, `{"error":"identity_change","old":"a","new":"b"}`, redeemIdentityChange},
		{http.StatusForbidden, `{"error":"channel","error_description":"rc required"}`, redeemTerminal},
		{http.StatusNotFound, `{"error":"unknown_flow"}`, redeemTerminal},
		{http.StatusBadRequest, `{"error":"link_error","error_description":"access_denied"}`, redeemTerminal},
	}
	for _, tc := range cases {
		srv := scriptedRedeem(t, []int{tc.status}, []string{tc.body})
		res, err := doRedeem(ctx, hc, srv.URL, "tok", "flow-1", "rc-secret", false)
		if err != nil {
			t.Fatalf("status %d: %v", tc.status, err)
		}
		if res.outcome != tc.want {
			t.Errorf("status %d → outcome %v, want %v", tc.status, res.outcome, tc.want)
		}
	}
}

func TestDoRedeemIdentityChangeCarriesLogins(t *testing.T) {
	srv := scriptedRedeem(t, []int{http.StatusConflict}, []string{`{"error":"identity_change","old":"old-bob","new":"new-eve"}`})
	res, err := doRedeem(context.Background(), &http.Client{}, srv.URL, "tok", "flow-1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.oldLogin != "old-bob" || res.newLogin != "new-eve" {
		t.Errorf("old/new = %q/%q, want old-bob/new-eve", res.oldLogin, res.newLogin)
	}
}

func seedLink(t *testing.T, st store.Store, account, login, gid string) {
	t.Helper()
	now := time.Now().Unix()
	_, err := st.GitHubLinks().RedeemUpsert(context.Background(), store.GitHubLink{
		SecretID:             "gh:" + account,
		AccountID:            account,
		Host:                 "github.com",
		Login:                login,
		GithubUserID:         gid,
		AppClientID:          "Iv1.app",
		RefreshToken:         "r",
		RefreshExpiresAtUnix: time.Now().Add(720 * time.Hour).Unix(),
		AccessToken:          "a",
		AccessExpiresAtUnix:  time.Now().Add(8 * time.Hour).Unix(),
		TokenType:            "bearer",
		UpdatedAt:            now,
	})
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
}

func TestDoListReturnsSeededLink(t *testing.T) {
	srv, _, st := buildLinkAS(t)
	seedLink(t, st, "acct-alice", "alice", "424242")
	links, err := doList(context.Background(), &http.Client{}, srv.URL, "acct-alice")
	if err != nil {
		t.Fatalf("doList: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("len = %d, want 1", len(links))
	}
	if links[0].Login != "alice" || links[0].Status != "linked" || links[0].SecretID != "gh:acct-alice" {
		t.Errorf("link = %+v", links[0])
	}
}

func TestDoListEmptyForUnknownAccount(t *testing.T) {
	srv, _, _ := buildLinkAS(t)
	links, err := doList(context.Background(), &http.Client{}, srv.URL, "nobody")
	if err != nil {
		t.Fatalf("doList: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("len = %d, want 0", len(links))
	}
}

func TestPrintStatusVariants(t *testing.T) {
	var b strings.Builder
	printStatus(&b, ghLink{Login: "alice", Version: 2, Host: "github.com", Status: "linked"})
	printStatus(&b, ghLink{Login: "bob", Host: "github.com", Status: "relink_required"})
	printStatus(&b, ghLink{Login: "eve", Host: "github.com", Status: "revoked"})
	out := b.String()
	for _, want := range []string{"Linked as @alice (v2)", "relink required", "revoked"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDoRevokeSucceeds(t *testing.T) {
	srv, _, st := buildLinkAS(t)
	seedLink(t, st, "acct-alice", "alice", "424242")
	if err := doRevoke(context.Background(), &http.Client{}, srv.URL, "acct-alice", "gh:acct-alice"); err != nil {
		t.Fatalf("doRevoke: %v", err)
	}
	// The link is now revoked (still listed, status revoked — revoked rows are not filtered out).
	links, err := doList(context.Background(), &http.Client{}, srv.URL, "acct-alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].Status != "revoked" {
		t.Errorf("after revoke links = %+v, want one revoked", links)
	}
}

func TestDoRevokeNotFound(t *testing.T) {
	srv, _, _ := buildLinkAS(t)
	err := doRevoke(context.Background(), &http.Client{}, srv.URL, "acct-alice", "gh:acct-alice")
	if err == nil {
		t.Fatal("expected error for missing link")
	}
}

func TestResolveRevokeSecretIDSingleLink(t *testing.T) {
	srv, _, st := buildLinkAS(t)
	seedLink(t, st, "acct-alice", "alice", "424242")
	id, err := resolveRevokeSecretID(context.Background(), &http.Client{}, srv.URL, "acct-alice", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "gh:acct-alice" {
		t.Errorf("id = %q, want gh:acct-alice", id)
	}
}

func TestResolveRevokeSecretIDNoLink(t *testing.T) {
	srv, _, _ := buildLinkAS(t)
	_, err := resolveRevokeSecretID(context.Background(), &http.Client{}, srv.URL, "acct-alice", "")
	if err == nil {
		t.Fatal("expected error when there is no link to revoke")
	}
}

func TestRenderDonePageStripsAndConfinesRC(t *testing.T) {
	rec := httptest.NewRecorder()
	renderDonePage(rec)
	body := rec.Body.String()
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", rec.Header().Get("Referrer-Policy"))
	}
	if !strings.Contains(body, "history.replaceState") {
		t.Error("page must strip rc from the address bar via history.replaceState")
	}
	// Self-contained: no external subresources.
	for _, bad := range []string{"http://", "https://", "src=", "href=http"} {
		if strings.Contains(body, bad) {
			t.Errorf("page is not self-contained: contains %q", bad)
		}
	}
}

func TestDoneHandlerForwardsRCWithoutReflecting(t *testing.T) {
	rcCh := make(chan string, 1)
	h := func(w http.ResponseWriter, r *http.Request) {
		renderDonePage(w)
		select {
		case rcCh <- r.URL.Query().Get("rc"):
		default:
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/done?rc=topsecret-completer", nil)
	h(rec, req)
	if got := <-rcCh; got != "topsecret-completer" {
		t.Errorf("forwarded rc = %q", got)
	}
	if strings.Contains(rec.Body.String(), "topsecret-completer") {
		t.Error("rc must NOT be reflected into the page body")
	}
}

func TestLinkLoopbackHappyPath(t *testing.T) {
	srv, fake, _ := buildLinkAS(t)
	fake.SetUser(424242, "alice")
	var out strings.Builder
	src := stubBearer{tok: "acct-alice"}
	hc := &http.Client{Timeout: 5 * time.Second}

	// The "browser opener" drives the OAuth chain (which redirects to 127.0.0.1:<port>/done?rc=).
	open := func(u string) error {
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	if err := linkLoopback(context.Background(), &out, src, hc, srv.URL, "", false, open); err != nil {
		t.Fatalf("linkLoopback: %v", err)
	}
	if !strings.Contains(out.String(), "Linked GitHub account @alice") {
		t.Errorf("output missing success line:\n%s", out.String())
	}
	if strings.Contains(out.String(), "rc=") || strings.Contains(out.String(), "?rc") {
		t.Errorf("rc must never be logged:\n%s", out.String())
	}
}

// stubBearer is a fixed-token bearerSource for link tests.
type stubBearer struct {
	tok string
}

func (s stubBearer) token(context.Context) (string, error) { return s.tok, nil }
func (s stubBearer) refresh(context.Context) error         { return nil }

// countingBearer records refresh calls and rotates the token on refresh.
type countingBearer struct {
	tok         string
	refreshHits int
}

func (c *countingBearer) token(context.Context) (string, error) { return c.tok, nil }
func (c *countingBearer) refresh(context.Context) error {
	c.refreshHits++
	c.tok = "refreshed-token"
	return nil
}

func withFastPoll(t *testing.T) {
	t.Helper()
	prev := ghDevicePollInterval
	ghDevicePollInterval = 5 * time.Millisecond
	t.Cleanup(func() { ghDevicePollInterval = prev })
}

func TestLinkDevicePollSucceedsAfterRefresh(t *testing.T) {
	withFastPoll(t)
	// 202 (pending), 401 (refresh+retry), 200 (done).
	srv := scriptedRedeem(t,
		[]int{http.StatusAccepted, http.StatusUnauthorized, http.StatusOK},
		[]string{`{"status":"pending"}`, `{"error":"unauthorized"}`,
			`{"secret_id":"gh:acct-alice","host":"github.com","login":"alice","github_user_id":"424242","version":1,"status":"linked"}`},
	)
	var out strings.Builder
	src := &countingBearer{tok: "acct-alice"}
	if err := linkDevice(context.Background(), &out, src, &http.Client{}, srv.URL, "", false); err != nil {
		t.Fatalf("linkDevice: %v", err)
	}
	if src.refreshHits != 1 {
		t.Errorf("refreshHits = %d, want 1", src.refreshHits)
	}
	if !strings.Contains(out.String(), "Linked GitHub account @alice") {
		t.Errorf("missing success line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "SECURITY:") {
		t.Errorf("consent warning not printed:\n%s", out.String())
	}
}

func TestLinkDeviceIdentityChange(t *testing.T) {
	withFastPoll(t)
	srv := scriptedRedeem(t,
		[]int{http.StatusConflict},
		[]string{`{"error":"identity_change","old":"old-bob","new":"new-eve"}`},
	)
	var out strings.Builder
	err := linkDevice(context.Background(), &out, &countingBearer{tok: "t"}, &http.Client{}, srv.URL, "", false)
	if err == nil || !strings.Contains(err.Error(), "old-bob") || !strings.Contains(err.Error(), "new-eve") {
		t.Fatalf("err = %v, want identity-change with old/new logins", err)
	}
}

func TestLinkDeviceTerminalFailFast(t *testing.T) {
	withFastPoll(t)
	srv := scriptedRedeem(t,
		[]int{http.StatusBadRequest},
		[]string{`{"error":"link_error","error_description":"access_denied"}`},
	)
	var out strings.Builder
	err := linkDevice(context.Background(), &out, &countingBearer{tok: "t"}, &http.Client{}, srv.URL, "", false)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v, want terminal access_denied", err)
	}
}

func TestSelectDeviceFlow(t *testing.T) {
	cases := []struct {
		device, noBrowser, browser bool
		want                       bool // want device
	}{
		{device: true, browser: true, want: true},
		{noBrowser: true, browser: true, want: true},
		{browser: true, want: false}, // browser reachable, no flags → loopback
		{browser: false, want: true}, // headless → device
	}
	for _, tc := range cases {
		if got := selectDeviceFlow(tc.device, tc.noBrowser, tc.browser); got != tc.want {
			t.Errorf("selectDeviceFlow(device=%v,noBrowser=%v,browser=%v) = %v, want %v",
				tc.device, tc.noBrowser, tc.browser, got, tc.want)
		}
	}
}

func TestGhLinkCmdFlags(t *testing.T) {
	c := ghLinkCmd()
	if c.Name != "link" {
		t.Fatalf("name = %q", c.Name)
	}
	names := map[string]bool{}
	for _, f := range c.Flags {
		for _, n := range f.Names() {
			names[n] = true
		}
	}
	for _, want := range []string{"as", "device", "no-browser", "host", "confirm-switch", "config-dir"} {
		if !names[want] {
			t.Errorf("missing flag %q", want)
		}
	}
	// The single-user-host precondition (spike S3) must be documented in the help.
	if !strings.Contains(c.Description, "single") && !strings.Contains(c.Usage, "single") {
		t.Errorf("link help must document the single-user-host precondition (S3)")
	}
}
