package githubfake

import (
	"bytes"
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

func TestFakeDeleteGrantRevokesWholeAuthorization(t *testing.T) {
	f := New()
	defer f.Close()

	// Mint an access+refresh pair via authorize→exchange so the fake has a live grant.
	verifier := "verif"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	redirectURI := "https://as.example.test/cb"
	code := authorizeCode(t, f, challenge)

	form := url.Values{
		"client_id":     {f.ClientID},
		"client_secret": {f.ClientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}
	xr, err := http.Post(f.URL()+"/login/oauth/access_token", "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(xr.Body).Decode(&tok)
	xr.Body.Close()
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("incomplete tuple")
	}

	// DELETE /applications/{client_id}/grant with Basic auth + JSON {access_token}.
	body, _ := json.Marshal(map[string]string{"access_token": tok.AccessToken})
	dreq, _ := http.NewRequest(http.MethodDelete, f.URL()+"/applications/"+f.ClientID+"/grant",
		bytes.NewReader(body))
	dreq.SetBasicAuth(f.ClientID, f.ClientSecret)
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		t.Fatalf("delete grant: %v", err)
	}
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete grant status = %d, want 204", dresp.StatusCode)
	}
	dresp.Body.Close()

	// Grant-wide: the old access token is dead at /user.
	ureq, _ := http.NewRequest(http.MethodGet, f.URL()+"/user", nil)
	ureq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uresp, _ := http.DefaultClient.Do(ureq)
	if uresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/user after revoke = %d, want 401", uresp.StatusCode)
	}
	uresp.Body.Close()

	// The refresh token can no longer rotate.
	rform := url.Values{
		"client_id":     {f.ClientID},
		"client_secret": {f.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {tok.RefreshToken},
	}
	rr, _ := http.Post(f.URL()+"/login/oauth/access_token", "application/x-www-form-urlencoded",
		strings.NewReader(rform.Encode()))
	var rout struct {
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&rout)
	rr.Body.Close()
	if rout.Error != "bad_refresh_token" || rout.AccessToken != "" {
		t.Fatalf("refresh after revoke = %+v, want bad_refresh_token", rout)
	}
}

func TestFakeDeleteTokenInvalidatesAccessButNotRefresh(t *testing.T) {
	f := New()
	defer f.Close()

	// Mint an access+refresh pair.
	verifier := "verif-tok"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	code := authorizeCode(t, f, challenge)
	form := url.Values{
		"client_id":     {f.ClientID},
		"client_secret": {f.ClientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {"https://as.example.test/cb"},
	}
	xr, err := http.Post(f.URL()+"/login/oauth/access_token", "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(xr.Body).Decode(&tok)
	xr.Body.Close()
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("incomplete tuple")
	}

	// Precondition: access token works.
	ureq, _ := http.NewRequest(http.MethodGet, f.URL()+"/user", nil)
	ureq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uresp, _ := http.DefaultClient.Do(ureq)
	if uresp.StatusCode != http.StatusOK {
		t.Fatalf("/user before DELETE /token = %d, want 200", uresp.StatusCode)
	}
	uresp.Body.Close()

	// DELETE /applications/{client_id}/token — targeted: kills only the access token.
	tbody, _ := json.Marshal(map[string]string{"access_token": tok.AccessToken})
	dreq, _ := http.NewRequest(http.MethodDelete, f.URL()+"/applications/"+f.ClientID+"/token",
		bytes.NewReader(tbody))
	dreq.SetBasicAuth(f.ClientID, f.ClientSecret)
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		t.Fatalf("delete token: %v", err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete token status = %d, want 204", dresp.StatusCode)
	}

	// The access token is now dead at /user.
	ureq2, _ := http.NewRequest(http.MethodGet, f.URL()+"/user", nil)
	ureq2.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uresp2, _ := http.DefaultClient.Do(ureq2)
	if uresp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/user after delete token = %d, want 401", uresp2.StatusCode)
	}
	uresp2.Body.Close()

	// But the refresh chain is still alive — we can rotate.
	rform := url.Values{
		"client_id":     {f.ClientID},
		"client_secret": {f.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {tok.RefreshToken},
	}
	rr, err := http.Post(f.URL()+"/login/oauth/access_token", "application/x-www-form-urlencoded",
		strings.NewReader(rform.Encode()))
	if err != nil {
		t.Fatalf("refresh after delete token: %v", err)
	}
	var rout struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&rout)
	rr.Body.Close()
	if rout.AccessToken == "" || rout.Error != "" {
		t.Fatalf("refresh after delete token = %+v, want new access_token", rout)
	}

	// 404 on a second delete of the same (now-dead) access token.
	tbody2, _ := json.Marshal(map[string]string{"access_token": tok.AccessToken})
	dreq2, _ := http.NewRequest(http.MethodDelete, f.URL()+"/applications/"+f.ClientID+"/token",
		bytes.NewReader(tbody2))
	dreq2.SetBasicAuth(f.ClientID, f.ClientSecret)
	dresp2, _ := http.DefaultClient.Do(dreq2)
	dresp2.Body.Close()
	if dresp2.StatusCode != http.StatusNotFound {
		t.Fatalf("idempotent delete token status = %d, want 404", dresp2.StatusCode)
	}
}
