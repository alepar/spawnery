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
}

// New starts the fake with registered confidential-client credentials.
func New() *Fake {
	f := &Fake{
		ClientID:     "fake-client-id",
		ClientSecret: "fake-client-secret",
		user:         User{ID: 1000001, Login: "octocat"},
		codes:        map[string]*issuedCode{},
		tokens:       map[string]User{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", f.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", f.exchange)
	mux.HandleFunc("GET /user", f.userEndpoint)
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
	f.tokens[tok] = c.user
	writeJSON(w, http.StatusOK, map[string]string{"access_token": tok, "token_type": "bearer"})
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

func randHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
