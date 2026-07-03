// Package githubfake is an in-process fake of the three GitHub endpoints the AS uses
// (authorize redirect, code->token exchange, GET /user). All identity tests run against it, and
// AS_FAKE_GITHUB=1 boots one for `just dev` — the REAL provider client code path is exercised
// either way (it just points at the fake's base URLs).
//
// The fake enforces what GitHub enforces: client_id/redirect_uri at authorize, client_secret +
// code single-use + PKCE verifier at exchange (GitHub validates PKCE since 2025-07 — do not
// regress this), bearer auth at /user. The user object carries the NUMERIC id — the subject —
// plus the mutable login [AM9].
package githubfake

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
)

// User is a GitHub user fixture. ID is the immutable numeric id; Login is mutable/recyclable.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type issuedCode struct {
	challenge   string // S256 code_challenge recorded at authorize
	redirectURI string
	user        User
	used        bool
}

type refreshGrant struct {
	user   User
	access string
}

// fakeUserIDBase is added to the fnv32a hash of a login to derive a stable numeric id for a login
// that has no explicit seed. Chosen well above the historical default-user id (1000001) and any
// hand-picked test id, so auto-registered logins never collide with seeded/back-compat ids.
const fakeUserIDBase = 2_000_000

// DeriveUserID returns the deterministic numeric id auto-assigned to a login with no explicit
// seed: fakeUserIDBase + fnv32a(login). Same login always yields the same id, across calls and
// process restarts. Exported so cmd/authsvc can pre-seed AS_FAKE_GITHUB_USERS entries that omit
// an explicit id and still agree with the fake's own login_hint auto-registration.
func DeriveUserID(login string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(login))
	return fakeUserIDBase + int64(h.Sum32())
}

// Options configures a Fake for non-default use: a reachable (non-loopback) bind address, an
// explicit advertised base URL, and/or seed users for login_hint selection. The zero value
// (Options{}) reproduces New()'s historical behavior exactly.
type Options struct {
	// Addr is the TCP bind address, e.g. "0.0.0.0:9099". Empty (the default) means loopback-random
	// (httptest.NewServer's own behavior).
	Addr string
	// BaseURL is the base URL advertised for every fake endpoint (authorize/token/user) — used for
	// both the AS's server-to-server calls and the browser-facing redirect, so both horizons
	// resolve from the same reachable host. Required whenever Addr is non-empty: httptest can't
	// derive a usable URL from a wildcard/unspecified bind.
	BaseURL string
	// Users seeds the login registry. Users[0] is also the default user (what SetUser overrides
	// and what an authorize call with no login_hint resolves to); if empty, the default user is
	// octocat (1000001), matching New().
	Users []User
}

// Fake is the in-process GitHub. authorize resolves the logging-in user from the request's
// login_hint query param: empty selects the default user (set at construction, or via SetUser);
// a non-empty hint selects a seeded user (AddUser/Options.Users) or auto-registers one
// deterministically (DeriveUserID). Force the next authorize's access_denied with DenyNext.
type Fake struct {
	Srv          *httptest.Server
	ClientID     string
	ClientSecret string

	mu          sync.Mutex
	defaultUser User
	users       map[string]User // login -> user, for login_hint resolution
	denyNext    bool
	codes       map[string]*issuedCode
	tokens      map[string]User
	refresh     map[string]refreshGrant

	baseURL string // advertised base URL; "" means derive from Srv.URL
}

// New starts the fake with registered confidential-client credentials: loopback bind, default
// user octocat (1000001). Equivalent to NewWithOptions(Options{}).
func New() *Fake {
	f, err := newFake(Options{})
	if err != nil {
		// Options{} never fails validation (Addr is empty) — unreachable in practice.
		panic(err)
	}
	return f
}

// NewWithOptions starts the fake per Options (see Options doc). Panics if the options are
// invalid (e.g. Addr set without BaseURL) or the requested Addr can't be bound — per the project's
// fail-loudly-never-skip ethos, a broken reachable-mode request is an error, not a silent
// loopback fallback.
func NewWithOptions(o Options) *Fake {
	f, err := newFake(o)
	if err != nil {
		panic(err)
	}
	return f
}

func newFake(o Options) (*Fake, error) {
	if o.Addr != "" && o.BaseURL == "" {
		return nil, fmt.Errorf("githubfake: Addr %q set without BaseURL — a non-default bind requires an explicit advertised BaseURL (httptest can't derive a usable URL from a wildcard/unspecified bind)", o.Addr)
	}

	defaultUser := User{ID: 1000001, Login: "octocat"}
	users := map[string]User{}
	if len(o.Users) > 0 {
		defaultUser = o.Users[0]
	}
	for _, u := range o.Users {
		users[u.Login] = u
	}
	// Seed defaultUser into users keyed by its login so login_hint=<defaultLogin> resolves to the
	// stable default id (1000001 for octocat) instead of auto-registering via DeriveUserID.
	users[defaultUser.Login] = defaultUser

	f := &Fake{
		ClientID:     "fake-client-id",
		ClientSecret: "fake-client-secret",
		defaultUser:  defaultUser,
		users:        users,
		baseURL:      o.BaseURL,
		codes:        map[string]*issuedCode{},
		tokens:       map[string]User{},
		refresh:      map[string]refreshGrant{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", f.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", f.exchange)
	mux.HandleFunc("GET /user", f.userEndpoint)
	mux.HandleFunc("DELETE /applications/{client_id}/grant", f.deleteGrant)
	mux.HandleFunc("DELETE /applications/{client_id}/token", f.deleteToken)

	if o.Addr == "" {
		f.Srv = httptest.NewServer(mux)
		return f, nil
	}

	srv := httptest.NewUnstartedServer(mux)
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return nil, fmt.Errorf("githubfake: listen %s: %w", o.Addr, err)
	}
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	f.Srv = srv
	return f, nil
}

func (f *Fake) Close() { f.Srv.Close() }

// URL returns the fake's advertised base URL: Options.BaseURL when set, else the loopback
// httptest URL.
func (f *Fake) URL() string {
	if f.baseURL != "" {
		return f.baseURL
	}
	return f.Srv.URL
}

// SetUser sets the default user — who authorize logs in as when login_hint is empty — and
// re-seeds it into the login registry so login_hint=<login> resolves to the same id instead of
// auto-registering via DeriveUserID.
func (f *Fake) SetUser(id int64, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultUser = User{ID: id, Login: login}
	f.users[login] = f.defaultUser
}

// AddUser seeds an explicit login->user mapping for login_hint selection, without touching the
// default user. Concurrency-safe.
func (f *Fake) AddUser(id int64, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[login] = User{ID: id, Login: login}
}

// resolveUser returns the user selected by an authorize request's login_hint: empty hint ->
// defaultUser (back-compat with the pre-login_hint single-user behavior); a known login -> its
// registered user; an unknown, non-empty login -> auto-registered deterministically
// (DeriveUserID), inserted under lock so concurrent authorize calls for the same new login agree
// on one id (first-writer-wins).
func (f *Fake) resolveUser(loginHint string) User {
	f.mu.Lock()
	defer f.mu.Unlock()
	if loginHint == "" {
		return f.defaultUser
	}
	if u, ok := f.users[loginHint]; ok {
		return u
	}
	u := User{ID: DeriveUserID(loginHint), Login: loginHint}
	f.users[loginHint] = u
	return u
}

// DenyNext makes the next authorize redirect back with error=access_denied.
func (f *Fake) DenyNext() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denyNext = true
}

func (f *Fake) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != f.ClientID {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	out := u.Query()
	out.Set("state", q.Get("state"))

	f.mu.Lock()
	if f.denyNext {
		f.denyNext = false
		f.mu.Unlock()
		out.Set("error", "access_denied")
		u.RawQuery = out.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	}
	f.mu.Unlock()

	// Resolved (and, if new, auto-registered) independently under its own lock — race-free
	// per-request selection, no process-global mutable "current user".
	user := f.resolveUser(q.Get("login_hint"))

	code := randHex()
	f.mu.Lock()
	f.codes[code] = &issuedCode{
		challenge:   q.Get("code_challenge"),
		redirectURI: redirectURI,
		user:        user,
	}
	f.mu.Unlock()

	out.Set("code", code)
	u.RawQuery = out.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (f *Fake) exchange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("client_id") != f.ClientID || r.PostForm.Get("client_secret") != f.ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "incorrect_client_credentials"})
		return
	}
	if r.PostForm.Get("grant_type") == "refresh_token" {
		f.refreshAccessToken(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.codes[r.PostForm.Get("code")]
	if !ok || c.used {
		writeJSON(w, http.StatusOK, map[string]string{"error": "bad_verification_code"})
		return
	}
	c.used = true
	if c.challenge != "" {
		ver := r.PostForm.Get("code_verifier")
		sum := sha256.Sum256([]byte(ver))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			writeJSON(w, http.StatusOK, map[string]string{"error": "incorrect_client_credentials", "error_description": "pkce verifier mismatch"})
			return
		}
	}
	tok := "gho_" + randHex()
	refreshTok := "ghr_" + randHex()
	f.tokens[tok] = c.user
	f.refresh[refreshTok] = refreshGrant{user: c.user, access: tok}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":             tok,
		"expires_in":               28800,
		"refresh_token":            refreshTok,
		"refresh_token_expires_in": 15897600,
		"token_type":               "bearer",
	})
}

func (f *Fake) refreshAccessToken(w http.ResponseWriter, r *http.Request) {
	oldRefresh := r.PostForm.Get("refresh_token")
	f.mu.Lock()
	defer f.mu.Unlock()
	grant, ok := f.refresh[oldRefresh]
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{"error": "bad_refresh_token"})
		return
	}
	delete(f.refresh, oldRefresh)
	delete(f.tokens, grant.access)
	nextAccess := "ghu_" + randHex()
	nextRefresh := "ghr_" + randHex()
	f.tokens[nextAccess] = grant.user
	f.refresh[nextRefresh] = refreshGrant{user: grant.user, access: nextAccess}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":             nextAccess,
		"expires_in":               28800,
		"refresh_token":            nextRefresh,
		"refresh_token_expires_in": 15897600,
		"token_type":               "bearer",
	})
}

func (f *Fake) userEndpoint(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(strings.TrimPrefix(auth, "Bearer "), "token ")
	f.mu.Lock()
	u, ok := f.tokens[tok]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// deleteGrant models DELETE /applications/{client_id}/grant: Basic-auth confidential client,
// {"access_token": ...} body, grant-WIDE teardown (every access+refresh token for that user dies).
// 204 on success, 404 when the access token maps to no live grant.
func (f *Fake) deleteGrant(w http.ResponseWriter, r *http.Request) {
	cid, secret, ok := r.BasicAuth()
	if !ok || cid != f.ClientID || secret != f.ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	if r.PathValue("client_id") != f.ClientID {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.AccessToken == "" {
		http.Error(w, "bad body", http.StatusUnprocessableEntity)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	user, ok := f.tokens[body.AccessToken]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// Grant-wide teardown: delete every access token and refresh grant for this user.
	for tok, u := range f.tokens {
		if u.ID == user.ID {
			delete(f.tokens, tok)
		}
	}
	for rt, g := range f.refresh {
		if g.user.ID == user.ID {
			delete(f.refresh, rt)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteToken models DELETE /applications/{client_id}/token: Basic-auth confidential client,
// {"access_token": ...} body, targeted single-token teardown (grant/refresh chain untouched).
// 204 on success, 404 when the access token is not live.
func (f *Fake) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != f.ClientID || secret != f.ClientSecret {
		http.Error(w, "bad client auth", http.StatusUnauthorized)
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.AccessToken == "" {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	_, present := f.tokens[body.AccessToken]
	delete(f.tokens, body.AccessToken)
	f.mu.Unlock()
	if !present {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func randHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
