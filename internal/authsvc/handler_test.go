package authsvc_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
	"spawnery/internal/weborigin"
)

// TestHandlerOuterCORSPreservesCredentialedPreflight is a regression test for [WL6] AS CORS.
//
// When AS_ALLOWED_ORIGINS is set, cmd/authsvc/main.go wraps the handler with the generic
// weborigin.CORS layer. That layer must NOT intercept OPTIONS preflights to /refresh or /logout:
// those endpoints carry corsCredentialed, which supplies Access-Control-Allow-Credentials and
// the X-PoP-* allowed headers required by AM2/AM5. Without those headers the browser rejects
// the subsequent credentialed silent-refresh request.
//
// The test applies the two-layer stack as main.go does (outer CORS scoped away from credentialed
// paths) and verifies the preflight headers on both credentialed endpoints.
func TestHandlerOuterCORSPreservesCredentialedPreflight(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()

	root, _ := pki.NewRootCA("R")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	st := store.NewTestStore(t)
	_, sigKey, _ := ed25519.GenerateKey(rand.Reader)

	spaOrigin := "https://app.example.com"
	idp, err := authsvc.NewIdP(authsvc.IdPConfig{
		Store:               st,
		GitHub:              authsvc.NewGitHubProvider(fake.URL(), fake.URL(), fake.ClientID, fake.ClientSecret),
		SigningKey:           sigKey,
		GitHubRedirectURI:   "https://as.example.com/oauth/callback",
		SPAOrigin:           spaOrigin,
		RedirectURIs:        []string{"https://app.example.com/callback"},
		RegistrationEnabled: true,
		Now:                 func() time.Time { return time.Unix(1770000000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := authsvc.New(root.Cert, inter, authsvc.WithIdP(idp))

	// Apply the two-layer CORS stack exactly as cmd/authsvc/main.go does after the [WL6] fix:
	// a production AS_ALLOWED_ORIGINS that includes spaOrigin (so the outer layer sees an
	// allowed origin), but credentialed routes bypass the outer layer to preserve their own
	// Access-Control-Allow-Credentials + X-PoP-* headers.
	allow := weborigin.FromEnv(spaOrigin)
	svcHandler := svc.Handler()
	outerCORS := allow.CORS(svcHandler)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/refresh" || p == "/logout" {
			svcHandler.ServeHTTP(w, r)
			return
		}
		outerCORS.ServeHTTP(w, r)
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := &http.Client{}

	for _, path := range []string{"/refresh", "/logout"} {
		t.Run(path, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodOptions, srv.URL+path, nil)
			req.Header.Set("Origin", spaOrigin)
			req.Header.Set("Access-Control-Request-Method", "POST")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("preflight %s: want 204, got %d", path, resp.StatusCode)
			}
			if resp.Header.Get("Access-Control-Allow-Origin") != spaOrigin {
				t.Fatalf("preflight %s: ACAO=%q", path, resp.Header.Get("Access-Control-Allow-Origin"))
			}
			if resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
				t.Fatalf("preflight %s: missing Access-Control-Allow-Credentials (outer CORS intercepted the preflight)", path)
			}
			allowedHeaders := resp.Header.Get("Access-Control-Allow-Headers")
			for _, h := range []string{"X-PoP-Timestamp", "X-PoP-Nonce", "X-PoP-Sig"} {
				if !strings.Contains(allowedHeaders, h) {
					t.Fatalf("preflight %s: %s missing from Access-Control-Allow-Headers: %q", path, h, allowedHeaders)
				}
			}
		})
	}
}

// The AS exposes a health endpoint and serves its Root CA for distribution (clients pin it out-of-band;
// this endpoint is for bootstrap/ops).
func TestHandlerServesHealthAndRootCA(t *testing.T) {
	root, _ := pki.NewRootCA("R")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	srv := httptest.NewServer(authsvc.New(root.Cert, inter).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz: %v status=%v", err, resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/ca/root")
	if err != nil {
		t.Fatalf("/ca/root: %v", err)
	}
	body, _ := io.ReadAll(resp2.Body)
	got, err := pki.ParseCertPEM(body)
	if err != nil {
		t.Fatalf("/ca/root did not return a PEM cert: %v", err)
	}
	if !got.Equal(root.Cert) {
		t.Fatal("/ca/root returned a different cert than the AS root")
	}
}
