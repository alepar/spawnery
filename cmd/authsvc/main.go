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
//	  AS_DB_DSN                      SQLite DSN (default: file:/var/lib/authsvc/identity.db)
//	  AS_DB_DRIVER                   "sqlite" (only; kept for future pg expansion)
//
//	GitHub OAuth (required for real login; ignored if AS_FAKE_GITHUB=1):
//	  GITHUB_CLIENT_ID               GitHub App client_id
//	  GITHUB_CLIENT_SECRET           GitHub App client_secret
//	  GITHUB_WEB_URL                 Base URL for GitHub web (default: https://github.com)
//	  GITHUB_API_URL                 Base URL for GitHub API (default: https://api.github.com)
//
//	AS callback + SPA contract:
//	  AS_GITHUB_REDIRECT_URI         AS's /oauth/callback URL as registered at GitHub App
//	  AS_SPA_ORIGINS                 The SPA origin for credentialed CORS (single origin; AM2 mandates one canonical origin per AS)
//	  AS_REDIRECT_URIS               Comma-separated registered client redirect_uri allowlist
//	  AS_VERIFICATION_URI            Device-grant user confirmation URL (SPA's /device/verify page)
//
//	Access controls:
//	  REGISTRATION_ENABLED           "true"/"1" = allow new user registration (default: true)
//	  AS_MAX_FAMILIES                Concurrent refresh-family cap per account (default: 20)
package main

import (
	"context"
	"crypto/ed25519"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/pki"
)

func main() {
	svc, err := buildService()
	if err != nil {
		log.Fatalf("authsvc: %v", err)
	}

	addr := env("AS_LISTEN", "127.0.0.1:8090")
	srv := &http.Server{Addr: addr, Handler: svc.Handler()}

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

	// Identity store.
	idStore, err := store.Open(context.Background(), store.Config{
		Driver: env("AS_DB_DRIVER", "sqlite"),
		DSN:    env("AS_DB_DSN", "file:/var/lib/authsvc/identity.db?_pragma=foreign_keys(1)"),
	})
	if err != nil {
		return nil, err
	}

	// GitHub provider.
	var ghProvider authsvc.GitHubProvider
	if os.Getenv("AS_FAKE_GITHUB") == "1" {
		log.Printf("authsvc: AS_FAKE_GITHUB=1 — using in-process fake GitHub (dev/CI only)")
		fake := githubfake.New()
		ghProvider = authsvc.NewGitHubProvider(fake.URL(), fake.URL(), fake.ClientID, fake.ClientSecret)
	} else {
		ghProvider = authsvc.NewGitHubProvider(
			env("GITHUB_WEB_URL", "https://github.com"),
			env("GITHUB_API_URL", "https://api.github.com"),
			mustEnv("GITHUB_CLIENT_ID"),
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
		SigningKey:           sigKey,
		NextPubKeys:         nextPubs,
		GitHubRedirectURI:   env("AS_GITHUB_REDIRECT_URI", ""),
		SPAOrigin:           spaOrigin,
		RedirectURIs:        redirectURIs,
		VerificationURI:     env("AS_VERIFICATION_URI", ""),
		RegistrationEnabled: regEnabled,
		MaxFamilies:         maxFamilies,
	})
	if err != nil {
		return nil, err
	}

	return authsvc.New(root.Cert, inter,
		authsvc.WithSessionKey(sigKey),
		authsvc.WithIdP(idp),
	), nil
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
		log.Printf("authsvc: WARNING — AS_SESSION_KEY_PEM not set; generating ephemeral key (not production-safe)")
		_, k, err := ed25519.GenerateKey(nil)
		return k, err
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
