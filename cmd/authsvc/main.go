// Command authsvc runs the Spawnery Auth Service: the identity root of trust, deployed in its own
// container apart from the CP. It holds the Root CA cert, the self-hosted intermediate (cert + key),
// and a certified auth-artifact signing leaf. It provides:
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
//	AS_FAKE_GITHUB_ADDR     Bind addr for a reachable fake GitHub, e.g. "0.0.0.0:9099" (default: loopback-random,
//	                        matching AS_FAKE_GITHUB=1's historical behavior). Requires AS_FAKE_GITHUB_BASE_URL.
//	AS_FAKE_GITHUB_BASE_URL Base URL advertised for every fake GitHub endpoint (authorize/token/user), used for
//	                        both AS->fake calls and the browser-facing redirect. Required when AS_FAKE_GITHUB_ADDR
//	                        is set.
//	AS_FAKE_GITHUB_USERS    Comma-separated seed users for the fake's login_hint selection: "login[:id],...".
//	                        The id derives (githubfake.DeriveUserID) when omitted. First entry is the default user.
//
//	CA / PKI material (required unless AS_DEV=1):
//	  AS_ROOT_CA_PEM                 Path to Root CA cert PEM (default: /etc/spawnery/as/root-ca.pem)
//	  AS_INTERMEDIATE_CERT_PEM       Path to self-hosted intermediate cert PEM
//	  AS_INTERMEDIATE_KEY_PEM        Path to self-hosted intermediate key PEM
//
//	Certified auth-artifact signing credentials:
//	  AS_AUTH_SIGNING_ENVIRONMENT        Environment trust-domain label (for example, prod)
//	  AS_AUTH_SIGNING_ROOT_PEM           Path to the environment Spawnery root certificate
//	  AS_AUTH_SIGNING_CURRENT_KEY_PEM    Path to the current Ed25519 PKCS#8 leaf key
//	  AS_AUTH_SIGNING_CURRENT_CHAIN_PEM  Path to its leaf-first certificate chain, root omitted
//	  AS_AUTH_SIGNING_NEXT_KEY_PEM       Optional next leaf key; requires NEXT_CHAIN_PEM
//	  AS_AUTH_SIGNING_NEXT_CHAIN_PEM     Optional next leaf-first chain; requires NEXT_KEY_PEM
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
//	  AS_CP_URL                      CP internal URL for GitHub mint authorization/fanout.
//	  AS_CP_SERVER_NAME              Expected CP internal TLS DNS name.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	configfiles "spawnery/config"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/config"
	"spawnery/internal/mtls"
	"spawnery/internal/pki"
	"spawnery/internal/weborigin"

	"os/signal"
	"syscall"
)

// devInMemoryDSN is the in-memory SQLite DSN used in dev mode when no AS_DB_DSN is set.
// Must match the value in config/authsvc.dev.yaml.
const devInMemoryDSN = "file:authsvc-dev?mode=memory&cache=shared&_pragma=foreign_keys(1)"

func loadConfig() (*AS, error) {
	configDir, sets := config.StdFlags("authsvc", os.Args[1:])
	cfg, err := config.Load[AS]("authsvc", config.Options{
		Args:        os.Args[1:],
		Embedded:    configfiles.FS,
		SecretsFS:   configfiles.FS,
		ExternalDir: configDir,
		EnvAliases:  asEnvAliases,
		Sets:        sets,
	})
	if err != nil {
		return nil, err
	}
	cfg.derive()
	return cfg, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("authsvc: config: %v", err)
	}

	certificateRevocations := pki.CertificateRevocationChecker(failClosedCertificateRevocations)
	var (
		certificateState *pki.RevocationState
		internalTLS      *tls.Config
		internalVerifier *mtls.PeerVerifier
		cpHTTPClient     *http.Client
	)
	if cfg.Internal.Listen != "" {
		certificateState, err = loadCertificateRevocations(cfg.Internal, time.Now)
		if err != nil {
			log.Fatalf("authsvc: %v", err)
		}
		defer func() { _ = certificateState.Close() }()
		certificateRevocations = certificateState.IsRevoked
		var root *x509.Certificate
		var identity tls.Certificate
		internalTLS, internalVerifier, root, identity, err = loadInternalTLSConfig(cfg.Internal, certificateState)
		if err != nil {
			log.Fatalf("authsvc: %v", err)
		}
		if !cfg.Dev {
			caRootPEM, readErr := os.ReadFile(cfg.CA.RootPEM)
			if readErr != nil {
				log.Fatalf("authsvc: read CA root for internal identity comparison: %v", readErr)
			}
			caRoot, parseErr := pki.ParseCertPEM(caRootPEM)
			if parseErr != nil || !bytes.Equal(caRoot.Raw, root.Raw) {
				log.Fatalf("authsvc: internal.root_ca must match ca.root_pem")
			}
		}
		if cfg.CP.URL != "" {
			cpHTTPClient, err = newInternalClient(root, identity, cfg.Internal.TrustDomain, cfg.CP.ServerName, pki.RoleCP, certificateState)
			if err != nil {
				log.Fatalf("authsvc: CP mTLS client: %v", err)
			}
		}
	}

	svc, err := buildService(cfg, certificateRevocations, cpHTTPClient)
	if err != nil {
		log.Fatalf("authsvc: %v", err)
	}

	// Browser-origin allowlist, same mechanism as the CP's ([WL6]): every device-set RPC is a
	// browser->AS call. Empty = dev mode (localhost origins only).
	allow := weborigin.FromEnv(cfg.AllowedOrigins)
	if allow.Dev() {
		log.Printf("authsvc: AS_ALLOWED_ORIGINS unset — dev mode, allowing loopback + private-network (LAN) browser origins only")
	}

	addr := cfg.Listen
	svcHandler := svc.Handler()
	// /refresh and /logout own their CORS via corsCredentialed, which supplies
	// Access-Control-Allow-Credentials and the X-PoP-* allowed headers required by AM2/AM5.
	// The outer weborigin.CORS layer lacks both; if it intercepts OPTIONS preflights for those
	// paths (which it does when the origin is in AS_ALLOWED_ORIGINS), the browser rejects the
	// subsequent credentialed request. Route credentialed paths directly to the inner handler.
	outerCORS := allow.CORS(svcHandler)
	routed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/refresh" || p == "/logout" {
			svcHandler.ServeHTTP(w, r)
			return
		}
		outerCORS.ServeHTTP(w, r)
	})
	// The public compatibility listener retains h2c for local tooling; internal traffic uses the
	// separate direct TLS listener below.
	publicServer := &http.Server{
		Addr:    addr,
		Handler: h2c.NewHandler(routed, &http2.Server{}),
	}
	var internalServer *http.Server
	if cfg.Internal.Listen != "" {
		internalHandler := mtls.PrincipalMiddleware(internalVerifier, svc.InternalHandler(authsvc.DefaultInternalPolicy()))
		internalServer, err = newInternalHTTPServer(cfg.Internal.Listen, internalHandler, internalTLS)
		if err != nil {
			log.Fatalf("authsvc: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = publicServer.Shutdown(sd)
		if internalServer != nil {
			_ = internalServer.Shutdown(sd)
		}
	}()

	if certificateState != nil {
		go refreshCertificateCRLs(ctx, certificateState, splitCSV(cfg.Internal.RevocationCRLs), cfg.Internal.RevocationRefreshInterval)
	}
	errors := make(chan error, 2)
	go func() { errors <- publicServer.ListenAndServe() }()
	if internalServer != nil {
		go func() { errors <- internalServer.ListenAndServeTLS("", "") }()
		log.Printf("authsvc internal mTLS listening on %s", cfg.Internal.Listen)
	}
	log.Printf("authsvc public listening on %s", addr)
	if serveErr := <-errors; serveErr != nil && serveErr != http.ErrServerClosed {
		log.Fatalf("authsvc: %v", serveErr)
	}
}

// buildService loads the AS's material and returns a fully-wired Service.
// AS_DEV=1 (cfg.Dev=true) bootstraps an ephemeral in-memory CA + fake GitHub (for `just dev`; NOT production).
func buildService(cfg *AS, certificateRevocations pki.CertificateRevocationChecker, cpHTTPClient *http.Client) (*authsvc.Service, error) {
	var (
		root  *pki.CA
		inter *pki.CA
		err   error
	)

	devProvisionedCA := cfg.Dev && cfg.CA.RootPEM != "" && cfg.CA.IntermediateCert != "" && cfg.CA.IntermediateKey != ""
	if cfg.Dev && !devProvisionedCA {
		log.Printf("authsvc: DEV MODE — ephemeral in-memory CA (do NOT use in production)")
		root, err = pki.NewRootCA("Spawnery Dev Root")
		if err != nil {
			return nil, err
		}
		inter, err = root.NewIntermediate(pki.ClassSelfHosted, cfg.CA.TrustDomain)
		if err != nil {
			return nil, err
		}
	} else {
		rc, ie := buildProductionCA(cfg)
		if ie != nil {
			return nil, ie
		}
		root, inter = rc.root, rc.inter
		if devProvisionedCA {
			log.Printf("authsvc: DEV MODE — using explicitly provisioned development CA")
		}
	}

	var credentials *signingCredentials
	if cfg.Dev && cfg.Signing.CurrentKeyPEM == "" && cfg.Signing.CurrentChainPEM == "" && cfg.Signing.RootPEM == "" {
		environment := cfg.Signing.Environment
		if environment == "" {
			environment = "dev"
		}
		signer, err := authsvc.NewDevelopmentSigningCredential(root, environment, time.Now())
		if err != nil {
			return nil, fmt.Errorf("authsvc: development signing bootstrap: %w", err)
		}
		credentials = &signingCredentials{Root: root.Cert, Current: signer}
		cfg.Signing.Environment = environment
		log.Printf("authsvc: DEV — generated ephemeral certified auth-artifact signer")
	} else {
		credentials, err = loadSigningCredentials(cfg.Signing, time.Now())
		if err != nil {
			return nil, err
		}
	}
	if credentials.Root != nil && !bytes.Equal(credentials.Root.Raw, root.Cert.Raw) {
		return nil, fmt.Errorf("authsvc: signing.root_pem does not match ca.root_pem")
	}
	artifactVerifier, err := token.NewVerifier(root.Cert, cfg.Signing.Environment, nil)
	if err != nil {
		return nil, fmt.Errorf("authsvc: artifact verifier: %w", err)
	}

	// Identity store. Dev mode defaults to an ephemeral in-memory DB, matching the dev CA and
	// dev session key — the prod default path is root-owned and must not be a dev dependency.
	dsn := cfg.DB.DSN
	if cfg.Dev && dsn == devInMemoryDSN {
		log.Printf("authsvc: DEV — ephemeral in-memory identity store (set AS_DB_DSN to persist)")
	}
	tokenCipher, err := loadGitHubTokenCipher(cfg)
	if err != nil {
		return nil, err
	}
	idStore, err := store.Open(context.Background(), store.Config{
		Driver:      cfg.DB.Driver,
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
	if cfg.FakeGithub || (cfg.Dev && cfg.GitHub.ClientID == "") {
		seedUsers, err := parseFakeGitHubUsers(cfg.FakeGitHubUsers)
		if err != nil {
			return nil, fmt.Errorf("authsvc: %w", err)
		}
		fake := githubfake.NewWithOptions(githubfake.Options{
			Addr:    cfg.FakeGitHubAddr,
			BaseURL: cfg.FakeGitHubBaseURL,
			Users:   seedUsers,
		})
		if cfg.FakeGitHubAddr != "" {
			log.Printf("authsvc: using in-process fake GitHub (dev/CI only), reachable at %s", fake.URL())
		} else {
			log.Printf("authsvc: using in-process fake GitHub (dev/CI only)")
		}
		ghProvider = authsvc.NewGitHubProvider(fake.URL(), fake.URL(), fake.ClientID, fake.ClientSecret)
		ghAppClientID = fake.ClientID
	} else {
		ghAppClientID = cfg.GitHub.ClientID
		ghProvider = authsvc.NewGitHubProvider(
			cfg.GitHub.WebURL,
			cfg.GitHub.APIURL,
			ghAppClientID,
			string(cfg.GitHub.ClientSecret),
		)
	}

	// Registration flag.
	regEnabled := cfg.RegistrationEnabled

	// Max families.
	maxFamilies := cfg.MaxFamilies

	// SPA origins + redirect URIs.
	spaOrigin := cfg.SPAOrigins
	// Take the first origin as the primary (credentialed CORS; single-origin per AM2).
	if idx := strings.IndexByte(spaOrigin, ','); idx >= 0 {
		spaOrigin = spaOrigin[:idx]
	}
	redirectURIs := splitCSV(cfg.RedirectURIs)

	idp, err := authsvc.NewIdP(authsvc.IdPConfig{
		Store:               idStore,
		GitHub:              ghProvider,
		Signer:              credentials.Current,
		NextSigner:          credentials.Next,
		GitHubRedirectURI:   cfg.GitHub.RedirectURI,
		SPAOrigin:           spaOrigin,
		RedirectURIs:        redirectURIs,
		VerificationURI:     cfg.VerificationURI,
		RegistrationEnabled: regEnabled,
		MaxFamilies:         maxFamilies,
	})
	if err != nil {
		return nil, err
	}

	opts := []authsvc.Option{
		authsvc.WithTrustDomain(cfg.CA.TrustDomain),
		authsvc.WithCertificateRevocations(certificateRevocations),
		authsvc.WithIdP(idp),
		authsvc.WithEnrollmentTokenIssuance(authsvc.EnrollmentSessionAccount(artifactVerifier, time.Now)),
		authsvc.WithNodeRevocations(idStore.NodeRevocations()),
		authsvc.WithGitHubMinting(idStore, ghProvider),
	}
	if cpURL := strings.TrimSpace(cfg.CP.URL); cpURL != "" {
		if cpHTTPClient == nil {
			return nil, errors.New("authsvc: CP URL requires internal mTLS client")
		}
		cpClient := cpv1connect.NewSpawnServiceClient(cpHTTPClient, cpURL)
		opts = append(opts,
			authsvc.WithGitHubMintAuthorizer(authsvc.NewCPGitHubMintAuthorizer(cpClient)),
			authsvc.WithGitHubTokenRotatedNotifier(authsvc.NewCPGitHubTokenRotatedNotifier(cpClient)),
		)
		log.Printf("authsvc: GitHub mint authorization/fanout wired to CP %s", cpURL)
	}

	// GitHub link bootstrap flow. Active only when github.link_redirect_uri is set — a distinct
	// callback from the login /oauth/callback (github.redirect_uri). Non-GitHub lanes leave this
	// unset and the /github/link/* handlers remain dormant.
	if linkRedirect := strings.TrimSpace(cfg.GitHub.LinkRedirectURI); linkRedirect != "" {
		exchanger, ok := ghProvider.(authsvc.GitHubLinkExchanger)
		if !ok {
			return nil, fmt.Errorf("authsvc: github provider does not implement GitHubLinkExchanger")
		}
		opts = append(opts, authsvc.WithGitHubLink(authsvc.GitHubLinkConfig{
			Exchanger:          exchanger,
			Store:              idStore,
			AppClientID:        ghAppClientID,
			RedirectURI:        linkRedirect,
			PostRedeemRedirect: cfg.GitHub.PostRedeemRedirect,
			DefaultHost:        cfg.GitHub.DefaultHost,
			AccountFromReq:     authsvc.SessionBearerAccount(artifactVerifier, time.Now),
			SPAOrigin:          spaOrigin,
		}))
		log.Printf("authsvc: GitHub link bootstrap flow ACTIVE (callback %s)", linkRedirect)
	}

	service := authsvc.New(root.Cert, inter, opts...)
	if err := service.Validate(); err != nil {
		return nil, err
	}
	return service, nil
}

type productionCA struct {
	root  *pki.CA
	inter *pki.CA
}

type signingCredentials struct {
	Root    *x509.Certificate
	Current *token.SigningCredential
	Next    *token.SigningCredential
}

func loadSigningCredentials(cfg ASAuthSigning, now time.Time) (*signingCredentials, error) {
	rootRaw, err := os.ReadFile(cfg.RootPEM)
	if err != nil {
		return nil, fmt.Errorf("authsvc: read signing.root_pem (%s): %w", cfg.RootPEM, err)
	}
	root, err := parseCertificatePEM(rootRaw)
	if err != nil {
		return nil, fmt.Errorf("authsvc: parse signing.root_pem (%s): %w", cfg.RootPEM, err)
	}
	current, err := loadSigningCredential("current", cfg.CurrentKeyPEM, cfg.CurrentChainPEM, root, cfg.Environment, now)
	if err != nil {
		return nil, err
	}
	credentials := &signingCredentials{Root: root, Current: current}
	if cfg.NextKeyPEM != "" || cfg.NextChainPEM != "" {
		if cfg.NextKeyPEM == "" || cfg.NextChainPEM == "" {
			return nil, fmt.Errorf("authsvc: signing next key and chain must be configured together")
		}
		credentials.Next, err = loadSigningCredential("next", cfg.NextKeyPEM, cfg.NextChainPEM, root, cfg.Environment, now)
		if err != nil {
			return nil, err
		}
	}
	return credentials, nil
}

func loadSigningCredential(label, keyPath, chainPath string, root *x509.Certificate, environment string, now time.Time) (*token.SigningCredential, error) {
	keyRaw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("authsvc: read %s signing key (%s): %w", label, keyPath, err)
	}
	defer clear(keyRaw)
	privateKey, err := parseSigningPrivateKeyPEM(keyRaw)
	if err != nil {
		return nil, fmt.Errorf("authsvc: parse %s signing key (%s): %w", label, keyPath, err)
	}
	defer clear(privateKey)
	chainRaw, err := os.ReadFile(chainPath)
	if err != nil {
		return nil, fmt.Errorf("authsvc: read %s signing chain (%s): %w", label, chainPath, err)
	}
	chain, err := parseCertificateChainPEM(chainRaw)
	if err != nil {
		return nil, fmt.Errorf("authsvc: parse %s signing chain (%s): %w", label, chainPath, err)
	}
	credential, err := token.NewSigningCredential(privateKey, chain, root, environment, now)
	if err != nil {
		return nil, fmt.Errorf("authsvc: validate %s signing credential (key %s, chain %s): %w", label, keyPath, chainPath, err)
	}
	return credential, nil
}

func parseCertificatePEM(raw []byte) (*x509.Certificate, error) {
	certificates, err := parseCertificateChainPEM(raw)
	if err != nil {
		return nil, err
	}
	if len(certificates) != 1 {
		return nil, fmt.Errorf("expected exactly one certificate, got %d", len(certificates))
	}
	return certificates[0], nil
}

func parseCertificateChainPEM(raw []byte) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	for len(raw) > 0 {
		block, rest, err := consumeCanonicalPEMBlock(raw, "CERTIFICATE")
		if err != nil {
			if len(certificates) > 0 {
				return nil, fmt.Errorf("invalid trailing PEM data: %w", err)
			}
			return nil, err
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		certificates = append(certificates, cert)
		raw = rest
	}
	if len(certificates) == 0 {
		return nil, fmt.Errorf("no certificates")
	}
	return certificates, nil
}

func parseSigningPrivateKeyPEM(raw []byte) (ed25519.PrivateKey, error) {
	block, rest, err := consumeCanonicalPEMBlock(raw, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	defer clear(block.Bytes)
	if block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(rest) != 0 {
		return nil, fmt.Errorf("expected exactly one headerless PRIVATE KEY block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key is not Ed25519")
	}
	canonical := ed25519.NewKeyFromSeed(privateKey[:ed25519.SeedSize])
	clear(privateKey)
	return canonical, nil
}

// consumeCanonicalPEMBlock decodes exactly one zero-offset PEM block. Both LF and CRLF are
// canonical inputs; mixed endings, prefixes, skipped malformed blocks, and trailing bytes within
// the framed block are rejected by byte-for-byte canonical re-encoding.
func consumeCanonicalPEMBlock(raw []byte, blockType string) (*pem.Block, []byte, error) {
	headerBase := "-----BEGIN " + blockType + "-----"
	newline := ""
	switch {
	case bytes.HasPrefix(raw, []byte(headerBase+"\n")):
		newline = "\n"
	case bytes.HasPrefix(raw, []byte(headerBase+"\r\n")):
		newline = "\r\n"
	default:
		return nil, nil, fmt.Errorf("PEM block does not start with %s", headerBase)
	}
	header := headerBase + newline
	footer := "-----END " + blockType + "-----" + newline
	body := raw[len(header):]
	footerOffset := bytes.Index(body, []byte(footer))
	if footerOffset < 0 || !hasPEMLineBoundary(body, footerOffset, newline) {
		return nil, nil, fmt.Errorf("invalid %s PEM block", blockType)
	}
	blockEnd := len(header) + footerOffset + len(footer)
	blockRaw := raw[:blockEnd]
	block, undecoded := pem.Decode(blockRaw)
	if block == nil || len(undecoded) != 0 {
		return nil, nil, fmt.Errorf("invalid %s PEM block", blockType)
	}
	canonical := pem.EncodeToMemory(block)
	if newline == "\r\n" {
		canonical = bytes.ReplaceAll(canonical, []byte("\n"), []byte("\r\n"))
	}
	if !bytes.Equal(blockRaw, canonical) {
		return nil, nil, fmt.Errorf("non-canonical %s PEM block", blockType)
	}
	return block, raw[blockEnd:], nil
}

func hasPEMLineBoundary(body []byte, footerOffset int, newline string) bool {
	if footerOffset == 0 {
		return true
	}
	return footerOffset >= len(newline) && bytes.Equal(body[footerOffset-len(newline):footerOffset], []byte(newline))
}

func buildProductionCA(cfg *AS) (*productionCA, error) {
	rootCertBytes, err := os.ReadFile(cfg.CA.RootPEM)
	if err != nil {
		return nil, fmt.Errorf("authsvc: read ca.root_pem (%s): %w", cfg.CA.RootPEM, err)
	}
	rootCert, err := pki.ParseCertPEM(rootCertBytes)
	if err != nil {
		return nil, err
	}
	interCertBytes, err := os.ReadFile(cfg.CA.IntermediateCert)
	if err != nil {
		return nil, fmt.Errorf("authsvc: read ca.intermediate_cert (%s): %w", cfg.CA.IntermediateCert, err)
	}
	interCert, err := pki.ParseCertPEM(interCertBytes)
	if err != nil {
		return nil, err
	}
	interKeyBytes, err := os.ReadFile(cfg.CA.IntermediateKey)
	if err != nil {
		return nil, fmt.Errorf("authsvc: read ca.intermediate_key (%s): %w", cfg.CA.IntermediateKey, err)
	}
	interKey, err := pki.ParseKeyPEM(interKeyBytes)
	if err != nil {
		return nil, err
	}
	return &productionCA{
		root:  &pki.CA{Cert: rootCert},
		inter: &pki.CA{Cert: interCert, Key: interKey},
	}, nil
}

// loadGitHubTokenCipher builds the at-rest cipher for AS-custodial github tokens
// (§16.2 / MAJOR-2). The key is held OUTSIDE the DB. Precedence:
//
//	github.token_enc_key      (standard-base64 32-byte key), else
//	github.token_enc_key_file (path to a file holding the base64 key), else
//	dev=true                  -> ephemeral random key (in-memory DB; data is ephemeral), else
//	error (fail-closed: prod must provide a key; Validate() enforces this).
func loadGitHubTokenCipher(cfg *AS) (store.TokenCipher, error) {
	if b64 := strings.TrimSpace(string(cfg.GitHub.TokenEncKey)); b64 != "" {
		return store.ParseTokenCipherKey(b64)
	}
	if path := strings.TrimSpace(cfg.GitHub.TokenEncKeyFile); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("authsvc: reading github.token_enc_key_file: %w", err)
		}
		return store.ParseTokenCipherKey(strings.TrimSpace(string(raw)))
	}
	if cfg.Dev {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		log.Printf("authsvc: DEV — ephemeral in-memory github token encryption key (set AS_GITHUB_TOKEN_ENC_KEY to persist)")
		return store.NewAESGCMTokenCipher(key)
	}
	// Validate() ensures this cannot happen in production.
	return nil, fmt.Errorf("authsvc: github.token_enc_key (or github.token_enc_key_file) is required for at-rest github token encryption")
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

// parseFakeGitHubUsers parses AS_FAKE_GITHUB_USERS ("login[:id],login[:id],...") into seed users
// for githubfake.Options.Users. An omitted id derives via githubfake.DeriveUserID — the same
// derivation the fake itself uses to auto-register an unseeded login_hint — so seeding here and
// auto-registration there always agree on one id for a given login.
func parseFakeGitHubUsers(csv string) ([]githubfake.User, error) {
	var users []githubfake.User
	for _, entry := range splitCSV(csv) {
		login, idStr, hasID := strings.Cut(entry, ":")
		login = strings.TrimSpace(login)
		if login == "" {
			return nil, fmt.Errorf("fake_github_users: empty login in %q", entry)
		}
		id := githubfake.DeriveUserID(login)
		if hasID {
			parsed, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("fake_github_users: bad id for login %q: %w", login, err)
			}
			id = parsed
		}
		users = append(users, githubfake.User{ID: id, Login: login})
	}
	return users, nil
}
