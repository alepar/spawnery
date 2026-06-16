package githubfake

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// noRedirect returns a client that surfaces 302s instead of following them.
func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func authorizeCode(t *testing.T, f *Fake, challenge string) string {
	t.Helper()
	q := url.Values{
		"client_id":             {f.ClientID},
		"redirect_uri":          {"http://client.example/cb"},
		"state":                 {"st"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	resp, err := noRedirect().Get(f.URL() + "/login/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("state") != "st" {
		t.Fatalf("state not echoed: %s", loc)
	}
	return loc.Query().Get("code")
}

func exchange(t *testing.T, f *Fake, code, secret, verifier string) map[string]string {
	t.Helper()
	form := url.Values{
		"client_id":     {f.ClientID},
		"client_secret": {secret},
		"code":          {code},
		"code_verifier": {verifier},
	}
	resp, err := http.PostForm(f.URL()+"/login/oauth/access_token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]string{"_status": resp.Status}
	_ = json.Unmarshal(b, &out)
	return out
}

func TestHappyExchangeAndUser(t *testing.T) {
	f := New()
	defer f.Close()
	f.SetUser(424242, "alice")

	verifier := "test-verifier-string-of-sufficient-length"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	code := authorizeCode(t, f, challenge)
	out := exchange(t, f, code, f.ClientSecret, verifier)
	tok := out["access_token"]
	if tok == "" {
		t.Fatalf("no access_token: %v", out)
	}

	req, _ := http.NewRequest(http.MethodGet, f.URL()+"/user", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var u User
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		t.Fatal(err)
	}
	if u.ID != 424242 || u.Login != "alice" {
		t.Fatalf("user: %+v", u)
	}

	// Code is single-use.
	if out := exchange(t, f, code, f.ClientSecret, verifier); out["access_token"] != "" {
		t.Fatalf("code reuse succeeded: %v", out)
	}
}

func TestRefreshInvalidatesPredecessorAccessAndRefresh(t *testing.T) {
	f := New()
	defer f.Close()
	f.SetUser(424242, "alice")

	verifier := "test-verifier-string-of-sufficient-length"
	sum := sha256.Sum256([]byte(verifier))
	code := authorizeCode(t, f, base64.RawURLEncoding.EncodeToString(sum[:]))
	out := exchange(t, f, code, f.ClientSecret, verifier)
	oldAccess := out["access_token"]
	oldRefresh := out["refresh_token"]
	if oldAccess == "" || oldRefresh == "" {
		t.Fatalf("exchange did not issue access+refresh: %v", out)
	}

	refreshForm := url.Values{
		"client_id":     {f.ClientID},
		"client_secret": {f.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {oldRefresh},
	}
	resp, err := http.PostForm(f.URL()+"/login/oauth/access_token", refreshForm)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var refreshed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed["access_token"] == "" || refreshed["refresh_token"] == "" {
		t.Fatalf("refresh did not issue successor tokens: %v", refreshed)
	}

	req, _ := http.NewRequest(http.MethodGet, f.URL()+"/user", nil)
	req.Header.Set("Authorization", "Bearer "+oldAccess)
	oldResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer oldResp.Body.Close()
	if oldResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("predecessor access token still valid: status %d", oldResp.StatusCode)
	}

	refreshForm.Set("refresh_token", oldRefresh)
	resp, err = http.PostForm(f.URL()+"/login/oauth/access_token", refreshForm)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reused map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&reused); err != nil {
		t.Fatal(err)
	}
	reusedAccess, _ := reused["access_token"].(string)
	if reusedAccess != "" || reused["error"] == nil {
		t.Fatalf("predecessor refresh token reuse succeeded: %v", reused)
	}
}

func TestPKCEMismatch(t *testing.T) {
	f := New()
	defer f.Close()
	sum := sha256.Sum256([]byte("right-verifier"))
	code := authorizeCode(t, f, base64.RawURLEncoding.EncodeToString(sum[:]))
	out := exchange(t, f, code, f.ClientSecret, "wrong-verifier")
	if out["access_token"] != "" || out["error"] == "" {
		t.Fatalf("pkce mismatch accepted: %v", out)
	}
}

func TestWrongSecret(t *testing.T) {
	f := New()
	defer f.Close()
	sum := sha256.Sum256([]byte("v"))
	code := authorizeCode(t, f, base64.RawURLEncoding.EncodeToString(sum[:]))
	out := exchange(t, f, code, "not-the-secret", "v")
	if out["access_token"] != "" || !strings.Contains(out["_status"], "401") {
		t.Fatalf("wrong secret accepted: %v", out)
	}
}

func TestDenyNext(t *testing.T) {
	f := New()
	defer f.Close()
	f.DenyNext()
	q := url.Values{"client_id": {f.ClientID}, "redirect_uri": {"http://client.example/cb"}, "state": {"s"}}
	resp, err := noRedirect().Get(f.URL() + "/login/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("error") != "access_denied" || loc.Query().Get("code") != "" {
		t.Fatalf("deny: %s", loc)
	}
}
