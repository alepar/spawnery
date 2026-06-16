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

// Fake is the in-process GitHub. Configure the next login's user via SetUser; force an
// access_denied with DenyNext.
type Fake struct {
	Srv          *httptest.Server
	ClientID     string
	ClientSecret string

	mu       sync.Mutex
	user     User
	denyNext bool
	codes    map[string]*issuedCode
	tokens   map[string]User
	refresh  map[string]refreshGrant
}

// New starts the fake with registered confidential-client credentials.
func New() *Fake {
	f := &Fake{
		ClientID:     "fake-client-id",
		ClientSecret: "fake-client-secret",
		user:         User{ID: 1000001, Login: "octocat"},
		codes:        map[string]*issuedCode{},
		tokens:       map[string]User{},
		refresh:      map[string]refreshGrant{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", f.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", f.exchange)
	mux.HandleFunc("GET /user", f.userEndpoint)
	mux.HandleFunc("DELETE /applications/{client_id}/grant", f.deleteGrant)
	f.Srv = httptest.NewServer(mux)
	return f
}

func (f *Fake) Close()      { f.Srv.Close() }
func (f *Fake) URL() string { return f.Srv.URL }

// SetUser sets the user the next logins authenticate as.
func (f *Fake) SetUser(id int64, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.user = User{ID: id, Login: login}
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
	code := randHex()
	f.codes[code] = &issuedCode{
		challenge:   q.Get("code_challenge"),
		redirectURI: redirectURI,
		user:        f.user,
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

func randHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
