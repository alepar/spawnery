// Command authsvc runs the Spawnery Auth Service: the identity root of trust, deployed in its own
// container apart from the CP. It holds the Root CA cert, the self-hosted intermediate (cert + key),
// and the AS session-signing key. It provides:
//   - Node enrollment (sp-0qc)
//   - AS-signed session tokens (sp-3ca)
//   - Identity: GitHub OAuth login, refresh families, device grant (sp-ussy.1)
//
// See docs/superpowers/specs/2026-06-11-auth-identity-design.md and deploy/authsvc/README.md.
//
// Environment variables:
//
//	AS_LISTEN               Address to listen on (default: 127.0.0.1:8090)
//	AS_DEV                  "1" = ephemeral in-memory CA + fake GitHub + dev session key (NOT for production)
//	AS_FAKE_GITHUB          "1" = use in-process fake GitHub provider (dev/CI; implies no real client creds)
//
//	CA / PKI material (required unless AS_DEV=1):
//	  AS_ROOT_CA_PEM                 Path to Root CA cert PEM (default: /etc/spawnery/as/root-ca.pem)
//	  AS_INTERMEDIATE_CERT_PEM       Path to self-hosted intermediate cert PEM
//	  AS_INTERMEDIATE_KEY_PEM        Path to self-hosted intermediate key PEM
//
//	Session signing keys (Ed25519, PKCS#8 PEM):
//	  AS_SESSION_KEY_PEM             Path to current session signing key (default: generated in AS_DEV)
//	  AS_SESSION_KEY_NEXT_PEM        Path to next session signing key (published for rotation; optional)
//
//	Database (sqlite tier-0; see deploy/authsvc/README.md for litestream replication):
//	  AS_DB_DSN                      SQLite DSN (default: file:/var/lib/authsvc/identity.db;
//	                                 AS_DEV=1 default: ephemeral in-memory)
//	  AS_DB_DRIVER                   "sqlite" (only; kept for future pg expansion)
//	  AS_GITHUB_TOKEN_ENC_KEY        Standard-base64 32-byte key for at-rest github token encryption
//	                                 (required in production; generated ephemerally in AS_DEV=1)
//	  AS_GITHUB_TOKEN_ENC_KEY_FILE   Path to a file holding the base64 key (alternative to _KEY)
//
//	GitHub OAuth (required for real login; ignored if AS_FAKE_GITHUB=1):
//	  GITHUB_CLIENT_ID               GitHub App client_id
//	  GITHUB_CLIENT_SECRET           GitHub App client_secret
//	  GITHUB_WEB_URL                 Base URL for GitHub web (default: https://github.com)
//	  GITHUB_API_URL                 Base URL for GitHub API (default: https://api.github.com)
//
//	AS callback + SPA contract:
//	  AS_GITHUB_REDIRECT_URI         AS's /oauth/callback URL as registered at GitHub App (login flow)
//	  AS_GITHUB_LINK_REDIRECT_URI    AS's /github/link/callback URL as registered at the GitHub App
//	                                 (activates the owner GitHub link flow; distinct from
//	                                 AS_GITHUB_REDIRECT_URI which is the login /oauth/callback)
//	  AS_GITHUB_POST_REDEEM_REDIRECT SPA page to land on after a successful link callback (optional)
//	  GITHUB_DEFAULT_HOST            Default git host for new links (default: github.com)
//	  AS_SPA_ORIGINS                 The SPA origin for credentialed CORS (single origin; AM2 mandates one canonical origin per AS)
//	  AS_REDIRECT_URIS               Comma-separated registered client redirect_uri allowlist
//	  AS_VERIFICATION_URI            Device-grant user confirmation URL (SPA's /device/verify page)
//
//	Access controls:
//	  REGISTRATION_ENABLED           "true"/"1" = allow new user registration (default: true)
//	  AS_MAX_FAMILIES                Concurrent refresh-family cap per account (default: 20)
//	  AS_CP_SECRET                   Shared bearer secret for GET /revocations (server-to-server; the CP
//	                                 must supply "Authorization: Bearer <secret>"). Required in production;
//	                                 leave unset only in AS_DEV. Without it the revocation feed (account
//	                                 UUIDs + session-revocation timing) is served unauthenticated.
//	  AS_CP_URL                      CP base URL for GitHub mint authorization/fanout.
//	  AS_CP_RPC_SECRET               Scoped AS->CP secret for GitHub coordination RPCs; must match
//	                                 CP_AS_RPC_SECRET on the CP.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/pki"
	"spawnery/internal/weborigin"
)

func main() {
	svc, err := buildService()
	if err != nil {
		log.Fatalf("authsvc: %v", err)
	}

	// Browser-origin allowlist, same mechanism as the CP's ([WL6]): every device-set RPC is a
	// browser->AS call. Empty = dev mode (localhost origins only).
	allow := weborigin.FromEnv(env("AS_ALLOWED_ORIGINS", ""))
	if allow.Dev() {
		log.Printf("authsvc: AS_ALLOWED_ORIGINS unset — dev mode, allowing loopback + private-network (LAN) browser origins only")
	}

	addr := env("AS_LISTEN", "127.0.0.1:8090")
	svcHandler := svc.Handler()
	// /refresh and /logout own their CORS via corsCredentialed, which supplies
	// Access-Control-Allow-Credentials and the X-PoP-* allowed headers required by AM2/AM5.
	// The outer weborigin.CORS layer lacks both; if it intercepts OPTIONS preflights for those
	// paths (which it does when the origin is in AS_ALLOWED_ORIGINS), the browser rejects the
	// subsequent credentialed request. Route credentialed paths directly to the inner handler.
	outerCORS := allow.CORS(svcHandler)
	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if p == "/refresh" || p == "/logout" {
				svcHandler.ServeHTTP(w, r)
				return
			}
			outerCORS.ServeHTTP(w, r)
		}),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sd)
	}()

	log.Printf("authsvc listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("authsvc: %v", err)
	}
}

// buildService loads the AS's material and returns a fully-wired Service.
// AS_DEV=1 bootstraps an ephemeral in-memory CA + fake GitHub (for `just dev`; NOT production).
func buildService() (*authsvc.Service, error) {
	var (
		root  *pki.CA
		inter *pki.CA
		err   error
	)

	if os.Getenv("AS_DEV") == "1" {
		log.Printf("authsvc: DEV MODE — ephemeral in-memory CA (do NOT use in production)")
		root, err = pki.NewRootCA("Spawnery Dev Root")
		if err != nil {
			return nil, err
		}
		inter, err = root.NewIntermediate(pki.ClassSelfHosted)
		if err != nil {
			return nil, err
		}
	} else {
		rc, ie := buildProductionCA()
		if ie != nil {
			return nil, ie
		}
		root, inter = rc.root, rc.inter
	}

	// Session signing key.
	sigKey, err := loadOrGenerateSigningKey()
	if err != nil {
		return nil, err
	}
	nextPubs, err := loadNextPubs()
	if err != nil {
		return nil, err
	}

	// Identity store. Dev mode defaults to an ephemeral in-memory DB, matching the dev CA and
	// dev session key — the prod default path is root-owned and must not be a dev dependency.
	defaultDSN := "file:/var/lib/authsvc/identity.db?_pragma=foreign_keys(1)"
	if os.Getenv("AS_DEV") == "1" {
		defaultDSN = "file:authsvc-dev?mode=memory&cache=shared&_pragma=foreign_keys(1)"
	}
	dsn := env("AS_DB_DSN", defaultDSN)
	if os.Getenv("AS_DEV") == "1" && dsn == defaultDSN {
		log.Printf("authsvc: DEV — ephemeral in-memory identity store (set AS_DB_DSN to persist)")
	}
	tokenCipher, err := loadGitHubTokenCipher()
	if err != nil {
		return nil, err
	}
	idStore, err := store.Open(context.Background(), store.Config{
		Driver:      env("AS_DB_DRIVER", "sqlite"),
		DSN:         dsn,
		TokenCipher: tokenCipher,
	})
	if err != nil {
		return nil, err
	}

	// GitHub provider. AS_DEV without real creds falls back to the in-process fake, so
	// `just dev` boots with zero GitHub setup (matching the header doc); real creds win.
	var ghProvider authsvc.GitHubProvider
	var ghAppClientID string
	if os.Getenv("AS_FAKE_GITHUB") == "1" || (os.Getenv("AS_DEV") == "1" && os.Getenv("GITHUB_CLIENT_ID") == "") {
		log.Printf("authsvc: using in-process fake GitHub (dev/CI only)")
		fake := githubfake.New()
		ghProvider = authsvc.NewGitHubProvider(fake.URL(), fake.URL(), fake.ClientID, fake.ClientSecret)
		ghAppClientID = fake.ClientID
	} else {
		ghAppClientID = mustEnv("GITHUB_CLIENT_ID")
		ghProvider = authsvc.NewGitHubProvider(
			env("GITHUB_WEB_URL", "https://github.com"),
			env("GITHUB_API_URL", "https://api.github.com"),
			ghAppClientID,
			mustEnv("GITHUB_CLIENT_SECRET"),
		)
	}

	// Registration flag.
	regEnabled := true
	if v := os.Getenv("REGISTRATION_ENABLED"); v == "false" || v == "0" {
		regEnabled = false
	}

	// Max families.
	maxFamilies := 20
	if v := os.Getenv("AS_MAX_FAMILIES"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			maxFamilies = n
		}
	}

	// SPA origins + redirect URIs.
	spaOrigin := env("AS_SPA_ORIGINS", "")
	// Take the first origin as the primary (credentialed CORS; single-origin per AM2).
	if idx := strings.IndexByte(spaOrigin, ','); idx >= 0 {
		spaOrigin = spaOrigin[:idx]
	}
	redirectURIs := splitCSV(env("AS_REDIRECT_URIS", ""))

	idp, err := authsvc.NewIdP(authsvc.IdPConfig{
		Store:               idStore,
		GitHub:              ghProvider,
		SigningKey:          sigKey,
		NextPubKeys:         nextPubs,
		GitHubRedirectURI:   env("AS_GITHUB_REDIRECT_URI", ""),
		SPAOrigin:           spaOrigin,
		RedirectURIs:        redirectURIs,
		VerificationURI:     env("AS_VERIFICATION_URI", ""),
		RegistrationEnabled: regEnabled,
		MaxFamilies:         maxFamilies,
		CPSecret:            os.Getenv("AS_CP_SECRET"),
	})
	if err != nil {
		return nil, err
	}

	opts := []authsvc.Option{
		authsvc.WithSessionKey(sigKey),
		authsvc.WithIdP(idp),
		authsvc.WithNodeRevocations(idStore.NodeRevocations()),
		authsvc.WithGitHubMinting(idStore, ghProvider),
	}
	if cpURL := strings.TrimSpace(os.Getenv("AS_CP_URL")); cpURL != "" {
		cpClient := cpv1connect.NewSpawnServiceClient(http.DefaultClient, cpURL,
			connect.WithInterceptors(staticHeaderInterceptor{
				name:  "X-Spawnery-AS-Secret",
				value: os.Getenv("AS_CP_RPC_SECRET"),
			}),
		)
		opts = append(opts,
			authsvc.WithGitHubMintAuthorizer(authsvc.NewCPGitHubMintAuthorizer(cpClient)),
			authsvc.WithGitHubAccessTokenFanout(authsvc.NewCPGitHubAccessTokenFanout(cpClient, pki.MarshalCertPEM(root.Cert), time.Now)),
		)
		log.Printf("authsvc: GitHub mint authorization/fanout wired to CP %s", cpURL)
	}

	// GitHub link bootstrap flow. Active only when AS_GITHUB_LINK_REDIRECT_URI is set — a
	// distinct callback from the login /oauth/callback (AS_GITHUB_REDIRECT_URI).  Non-GitHub
	// lanes leave this unset and the /github/link/* handlers remain dormant.
	if linkRedirect := strings.TrimSpace(os.Getenv("AS_GITHUB_LINK_REDIRECT_URI")); linkRedirect != "" {
		exchanger, ok := ghProvider.(authsvc.GitHubLinkExchanger)
		if !ok {
			return nil, fmt.Errorf("authsvc: github provider does not implement GitHubLinkExchanger")
		}
		opts = append(opts, authsvc.WithGitHubLink(authsvc.GitHubLinkConfig{
			Exchanger:          exchanger,
			Store:              idStore,
			AppClientID:        ghAppClientID,
			RedirectURI:        linkRedirect,
			PostRedeemRedirect: env("AS_GITHUB_POST_REDEEM_REDIRECT", ""),
			DefaultHost:        env("GITHUB_DEFAULT_HOST", "github.com"),
			AccountFromReq:     authsvc.SessionBearerAccount(idp.KeySet(), time.Now),
			SPAOrigin:          spaOrigin,
		}))
		log.Printf("authsvc: GitHub link bootstrap flow ACTIVE (callback %s)", linkRedirect)
	}

	return authsvc.New(root.Cert, inter, opts...), nil
}

type staticHeaderInterceptor struct {
	name  string
	value string
}

func (i staticHeaderInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if i.name != "" && i.value != "" {
			req.Header().Set(i.name, i.value)
		}
		return next(ctx, req)
	}
}

func (i staticHeaderInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if i.name != "" && i.value != "" {
			conn.RequestHeader().Set(i.name, i.value)
		}
		return conn
	}
}

func (i staticHeaderInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

type productionCA struct {
	root  *pki.CA
	inter *pki.CA
}

func buildProductionCA() (*productionCA, error) {
	rootCert, err := pki.ParseCertPEM(mustRead("AS_ROOT_CA_PEM", "/etc/spawnery/as/root-ca.pem"))
	if err != nil {
		return nil, err
	}
	interCert, err := pki.ParseCertPEM(mustRead("AS_INTERMEDIATE_CERT_PEM", "/etc/spawnery/as/self-hosted-intermediate.pem"))
	if err != nil {
		return nil, err
	}
	interKey, err := pki.ParseKeyPEM(mustRead("AS_INTERMEDIATE_KEY_PEM", "/etc/spawnery/as/self-hosted-intermediate-key.pem"))
	if err != nil {
		return nil, err
	}
	return &productionCA{
		root:  &pki.CA{Cert: rootCert},
		inter: &pki.CA{Cert: interCert, Key: interKey},
	}, nil
}

func loadOrGenerateSigningKey() (ed25519.PrivateKey, error) {
	path := os.Getenv("AS_SESSION_KEY_PEM")
	if path == "" {
		if os.Getenv("AS_DEV") == "1" {
			_, k, err := ed25519.GenerateKey(nil)
			if err != nil {
				return nil, err
			}
			log.Printf("authsvc: DEV — generated ephemeral session signing key")
			return k, nil
		}
		// Production: a missing session key is a fatal misconfiguration [AM13]. An ephemeral key
		// mints tokens the CP's pinned key-set cannot verify and that do not survive a restart.
		return nil, fmt.Errorf("AS_SESSION_KEY_PEM is required in production (set AS_DEV=1 for development)")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, _, err := token.LoadSigningKey(pemBytes)
	return k, err
}

func loadNextPubs() ([]ed25519.PublicKey, error) {
	path := os.Getenv("AS_SESSION_KEY_NEXT_PEM")
	if path == "" {
		return nil, nil
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pub, err := token.ParsePublicKeyPEM(pemBytes)
	if err != nil {
		return nil, err
	}
	return []ed25519.PublicKey{pub}, nil
}

// loadGitHubTokenCipher builds the at-rest cipher for AS-custodial github tokens
// (§16.2 / MAJOR-2). The key is held OUTSIDE the DB. Precedence:
//
//	AS_GITHUB_TOKEN_ENC_KEY      (standard-base64 32-byte key), else
//	AS_GITHUB_TOKEN_ENC_KEY_FILE (path to a file holding the base64 key), else
//	AS_DEV=1                     -> ephemeral random key (in-memory DB; data is ephemeral), else
//	error (fail-closed: prod must provide a key).
func loadGitHubTokenCipher() (store.TokenCipher, error) {
	if b64 := strings.TrimSpace(os.Getenv("AS_GITHUB_TOKEN_ENC_KEY")); b64 != "" {
		return store.ParseTokenCipherKey(b64)
	}
	if path := strings.TrimSpace(os.Getenv("AS_GITHUB_TOKEN_ENC_KEY_FILE")); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("authsvc: reading AS_GITHUB_TOKEN_ENC_KEY_FILE: %w", err)
		}
		return store.ParseTokenCipherKey(strings.TrimSpace(string(raw)))
	}
	if os.Getenv("AS_DEV") == "1" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		log.Printf("authsvc: DEV — ephemeral in-memory github token encryption key (set AS_GITHUB_TOKEN_ENC_KEY to persist)")
		return store.NewAESGCMTokenCipher(key)
	}
	return nil, fmt.Errorf("authsvc: AS_GITHUB_TOKEN_ENC_KEY (or _FILE) is required for at-rest github token encryption")
}

func mustRead(envKey, def string) []byte {
	path := env(envKey, def)
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("authsvc: read %s (%s): %v", envKey, path, err)
	}
	return b
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("authsvc: required env var %s not set", k)
	}
	return v
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
