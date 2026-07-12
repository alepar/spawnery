package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	configfiles "spawnery/config"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/gen/node/v1/nodev1connect"
	authservice "spawnery/internal/authsvc"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/config"
	"spawnery/internal/cp"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/nodeauth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/skillfetch"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/h2keepalive"
	"spawnery/internal/health"
	applog "spawnery/internal/log"
	"spawnery/internal/metrics"
	"spawnery/internal/mtls"
	"spawnery/internal/pki"
	"spawnery/internal/rpclog"
	"spawnery/internal/safego"
	"spawnery/internal/weborigin"
)

const sqliteDefaultDSN = "file:cp.db?_pragma=busy_timeout(5000)"

func loadConfig() (*CP, error) {
	configDir, sets := config.StdFlags("spawnery_cp", os.Args[1:])
	cfg, err := config.Load[CP]("cp", config.Options{
		Args:        os.Args[1:],
		Embedded:    configfiles.FS,
		SecretsFS:   configfiles.FS,
		ExternalDir: configDir,
		EnvAliases:  cpEnvAliases,
		Sets:        sets,
	})
	if err != nil {
		return nil, err
	}
	cfg.derive()
	return cfg, nil
}

func main() {
	applog.Init(os.Getenv)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("cp: config: %v", err)
	}

	reg := registry.New()
	rt := router.New()
	sched := scheduler.New(reg, rt, 60*time.Second)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- Auth mode ---
	// auth.mode: "dev" (default) | "prod". Production ignores opaque dev tokens.
	devMode := cfg.DevMode()
	if !devMode {
		log.Printf("cp: auth mode=prod (dev tokens ignored)")
	} else {
		log.Printf("cp: auth mode=dev (dev tokens active; NOT FOR PRODUCTION)")
	}

	artifacts, signerRevocations, err := loadArtifactVerifier(*cfg, time.Now())
	if err != nil {
		log.Fatalf("cp: load artifact verifier: %v", err)
	}
	var stopRevocationReloader func() bool
	if signerRevocations != nil && cfg.Auth.SignerRevocationStatement != "" {
		reloadCtx, cancelReload := context.WithCancel(ctx)
		reloadDone := make(chan struct{})
		reloader := newSignerRevocationReloader(signerRevocations, cfg.Auth.SignerRevocationStatement)
		safego.Go("cp.signer-revocation-reloader", func() {
			defer close(reloadDone)
			reloader.Run(reloadCtx)
		})
		stopRevocationReloader = func() bool {
			return stopSignerRevocationReloader(cancelReload, reloadDone, signerRevocationShutdownBound)
		}
	}
	if signerRevocations != nil {
		defer func() {
			if stopRevocationReloader != nil && !stopRevocationReloader() {
				log.Printf("cp: signer revocation reloader did not stop within %s; leaving store open for process exit", signerRevocationShutdownBound)
				return
			}
			if err := signerRevocations.Close(); err != nil {
				log.Printf("cp: close signer revocation store: %v", err)
			}
		}()
	}

	// --- Revocation + session registries ---
	sessions := auth.NewSessionRegistry()
	revreg := auth.NewRevocationRegistry(sessions)

	// --- Dev tokens (honored only in dev mode) ---
	devTokens := map[string]string{}
	if devMode {
		devTokens = parseTokens(cfg.Auth.DevTokens)
	}

	// --- Verifier ---
	verifier := auth.NewVerifier(auth.VerifierConfig{
		Artifacts: artifacts,
		DevTokens: devTokens,
		DevMode:   devMode,
		Revoked:   revreg,
	})

	st, err := store.Open(ctx, store.Config{Driver: cfg.Store.Driver, DSN: string(cfg.Store.DSN)})
	if err != nil {
		log.Fatalf("store open: %v", err)
	}
	defer st.Close()
	// TODO(sp-7hl): real definition repos per app — all four reuse examples/secret-app until then.
	seedApps := []cp.AppSeed{
		{ID: "spawnery/wiki", Ref: "examples/secret-app", Version: "1.0.0",
			DisplayName: "Wiki & Research Companion", Summary: "Capture articles, links, and notes; it extracts, connects, and recalls.",
			Tags: []string{"notes", "research", "second-brain"}, Mounts: []string{"main"}},
		{ID: "spawnery/language", Ref: "examples/secret-app", Version: "1.0.0",
			DisplayName: "Language-Learning Partner", Summary: "Tracks your vocab and mistakes; drills, converses, and adapts.",
			Tags: []string{"language", "tutor", "practice"}, Mounts: []string{"main"}},
		{ID: "spawnery/interview", Ref: "examples/secret-app", Version: "1.0.0",
			DisplayName: "Interview Coach (System Design)", Summary: "Mock interviews with structured, scored feedback over time.",
			Tags: []string{"interview", "coaching", "system-design"}, Mounts: []string{"main"}},
		{ID: "spawnery/zork", Ref: "examples/secret-app", Version: "1.0.0",
			DisplayName: "Zork", Summary: "The classic adventure — vertical-slice smoke test and toy.",
			Tags: []string{"game", "demo", "smoke-test"}, Mounts: []string{"main"}},
		{ID: "spawnery/secret-app", Ref: "examples/secret-app", Version: "1.0.0",
			DisplayName: "Secret App", Summary: "Vertical-slice smoke test — ask it for the secret word.",
			Tags: []string{"demo", "smoke-test"}, Mounts: []string{"main"}},
		{ID: "spawnery/github-app", Ref: "examples/github-app", Version: "1.0.0",
			DisplayName: "GitHub Repo Agent", Summary: "Clone a GitHub repo you pick into a journaled mount; the agent does git ops under your linked identity.",
			Tags: []string{"github", "demo", "dev-integration"}, Mounts: []string{"repo"}},
	}
	// Dev-owner seeding: only in dev mode (prod accountIds are created lazily by the AS).
	if devMode {
		if err := cp.Seed(ctx, st, devTokens, seedApps); err != nil {
			log.Fatalf("store seed: %v", err)
		}
	} else {
		if err := cp.SeedApps(ctx, st, seedApps); err != nil {
			log.Fatalf("store seed apps: %v", err)
		}
	}
	if n, err := st.Spawns().MarkBootUnreachable(ctx); err != nil {
		log.Fatalf("boot reconcile: %v", err)
	} else if n > 0 {
		log.Printf("boot reconcile: marked %d orphaned spawn(s) unreachable", n)
	}

	var tel telemetry.Sink = telemetry.NopSink{}
	if p := cfg.Telemetry; p != "" {
		if err := os.MkdirAll(dir(p), 0o755); err == nil {
			if js, err := telemetry.NewJSONLSink(p); err == nil {
				tel = js
				defer js.Close()
			} else {
				log.Printf("telemetry sink disabled: %v", err)
			}
		}
	}

	srv := cp.NewServer(reg, rt, sched, st, tel)
	srv.SetMaxSpawnsPerOwner(cfg.MaxSpawnsPerOwner)
	srv.SetSessionRegistry(sessions)
	srv.SetVerify(verifier.Verify)
	srv.SetDevMode(devMode)
	// CP-side metric evaluators (§6 transition-coordination-design): disabled by default.
	// evaluator.quota_suspend_mb > 0 and/or evaluator.idle_enabled activate them.
	if quotaMB := cfg.Evaluator.QuotaSuspendMB; quotaMB > 0 {
		srv.SetEvaluatorPolicy(cfg.Evaluator.IdleDetached, cfg.Evaluator.IdleAttached, quotaMB)
		log.Printf("evaluator: enabled quota=%dMiB idle_detached=%s idle_attached=%s", quotaMB, cfg.Evaluator.IdleDetached, cfg.Evaluator.IdleAttached)
	} else if cfg.Evaluator.IdleEnabled {
		srv.SetEvaluatorPolicy(cfg.Evaluator.IdleDetached, cfg.Evaluator.IdleAttached, 0)
		log.Printf("evaluator: idle-only enabled detached=%s attached=%s", cfg.Evaluator.IdleDetached, cfg.Evaluator.IdleAttached)
	}
	if ri := cfg.Auth.SessionReauthInterval; ri > 0 {
		srv.SetReauthInterval(ri)
	}

	// A4 intent flow setup [AC1][AM12].
	// Prod mode: intent flow always active; clients obtain node tokens from the real AS.
	// Dev mode: intent flow is OFF by default — the web SPA does not yet implement
	// GetPendingIntent/SubmitIntent (A5). The dev AS key is always provisioned so spawnctl's
	// pollAndSign works when opted in. Set auth.dev_intent_enabled=true to enable the two-phase
	// flow in dev; without it web-initiated spawns proceed with a nil env and the node runs
	// in verify-and-log mode.
	if !devMode {
		srv.SetIntentEnabled(true)
	} else {
		devRoot, devASErr := pki.NewRootCA("Spawnery CP development root")
		if devASErr != nil {
			log.Fatalf("cp: generate dev root: %v", devASErr)
		}
		devSigner, devASErr := authservice.NewDevelopmentSigningCredential(devRoot, "dev", time.Now())
		if devASErr != nil {
			log.Fatalf("cp: generate certified dev signer: %v", devASErr)
		}
		srv.SetDevASCredential(devSigner)
		log.Printf("cp: using ephemeral certified dev AS signer [AM12]")
		// auth.dev_intent_enabled: opt into the two-phase sign flow in dev mode.
		if cfg.Auth.DevIntentEnabled {
			srv.SetIntentEnabled(true)
			log.Printf("cp: dev intent flow enabled (auth.dev_intent_enabled=true) [AM12]")
		} else {
			log.Printf("cp: dev intent flow off (set auth.dev_intent_enabled=true to enable; web spawns proceed without signing) [AM12]")
		}
	}

	internalRuntime, err := loadInternalRuntime(*cfg, time.Now)
	if err != nil {
		log.Fatalf("cp: load internal mTLS: %v", err)
	}
	if internalRuntime != nil {
		defer internalRuntime.revocations.Close()
		if len(cfg.Internal.RevocationCRLs) > 0 {
			safego.Go("cp.certificate-revocation-reloader", func() {
				refreshCertificateRevocations(ctx, internalRuntime.revocations, cfg.Internal.RevocationCRLs, cfg.Internal.RevocationRefreshInterval)
			})
		}
	}

	// CP→AS GitHub link-status preflight uses the CP service SVID on the internal transport.
	// When set, CreateSpawn checks the owner's GitHub link state before persisting a github:-mount spawn.
	if asURL := strings.TrimSpace(cfg.Auth.ASURL); asURL != "" && !cfg.Auth.GitHubLinkPreflightDisabled {
		if internalRuntime == nil {
			log.Fatalf("cp: auth.as_url requires internal mTLS configuration")
		}
		srv.SetASLinkChecker(asURL, internalRuntime.client)
		log.Printf("cp: GitHub link preflight checker wired to AS %s", asURL)
	} else if cfg.Auth.GitHubLinkPreflightDisabled {
		log.Printf("cp: GitHub link preflight DISABLED (auth.github_link_preflight_disabled=true) — static-token git-host dev lane")
	}

	// URL skill ingest (sp-nrzf.3.14.4): wire Garage skill store + fetcher when configured.
	// skills.endpoint empty => IngestSkillFromURL returns FailedPrecondition (no Garage configured).
	// The S3 bucket (default "spawnery-skills") must be pre-provisioned out-of-band —
	// the CP's journal key is Forbidden for MakeBucket (spike S1 finding).
	if ep := strings.TrimSpace(string(cfg.Skills.Endpoint)); ep != "" {
		ssCfg := skillstore.Config{
			Endpoint:        ep,
			NodeEndpoint:    cfg.Skills.NodeEndpoint,
			AccessKeyID:     string(cfg.Skills.AccessKeyID),
			SecretAccessKey: string(cfg.Skills.SecretAccessKey),
			Region:          cfg.Skills.Region,
			DisableTLS:      cfg.Skills.DisableTLS,
			Bucket:          cfg.Skills.Bucket,
		}
		ss, err := skillstore.New(ssCfg)
		if err != nil {
			log.Fatalf("cp: build skill store: %v", err)
		}
		fetcher := skillfetch.New(skillfetch.Config{
			GitHubToken: string(cfg.Skills.GitHubToken),
			ZstdLevel:   cfg.Skills.ZstdLevel,
		})
		srv.SetSkillIngest(fetcher, ss)
		log.Printf("cp: skill ingest wired (endpoint=%s bucket=%s)", ep, ssCfg.Bucket)
	}

	srv.StartReconciler(ctx) // background loop: drive model_applied=false spawns to convergence (sp-bp9w.7)

	// Browser-origin allowlist for CORS + the WS upgrade ([WM18]). Empty = dev mode
	// (localhost-only origins); production sets the exact canonical SPA origin(s).
	allow := weborigin.FromEnv(cfg.AllowedOrigins)
	if allow.Dev() {
		log.Printf("cp: allowed_origins unset — dev mode, allowing loopback + private-network (LAN) browser origins only")
	}

	// Start revocation feed poller if configured.
	if feedURL := cfg.Auth.ASRevocationURL; feedURL != "" {
		if internalRuntime == nil {
			log.Fatalf("cp: auth.as_revocation_url requires internal mTLS configuration")
		}
		interval := cfg.Auth.RevocationPollInterval
		poller := auth.NewFeedPoller(internalRuntime.client, feedURL, artifacts, revreg, interval)
		safego.Go("cp.revocation-poller", func() { poller.Run(ctx) })
		log.Printf("cp: revocation feed poller started (url=%s interval=%s)", feedURL, interval)
	}

	nodePath, nodeHandler := nodev1connect.NewNodeServiceHandler(srv, connect.WithInterceptors(rpclog.CorrelationInterceptor(), metrics.RPCInterceptor(), rpclog.RecoverInterceptor("cp"), rpclog.Interceptor("cp")))
	var internalSrv *http.Server
	devNodePath := ""
	if internalRuntime != nil {
		_, internalSpawnHandler := cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(rpclog.CorrelationInterceptor(), metrics.RPCInterceptor(), rpclog.RecoverInterceptor("cp"), rpclog.Interceptor("cp")))
		internalSrv, err = buildInternalTLSServer(cfg.Internal.Listen, internalRuntime.tlsConfig, buildInternalHandler(internalRuntime.verifier, internalSpawnHandler, nodeHandler))
		if err != nil {
			log.Fatalf("cp: build internal mTLS listener: %v", err)
		}
		safego.Go("cp.internal-mtls-listener", func() {
			if err := internalSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				log.Fatalf("cp: internal mTLS listener: %v", err)
			}
		})
	} else {
		if cfg.Internal.InsecureDevNodeOnPublic {
			devNodePath = nodePath
		}
	}
	publicHandler := buildPublicHandler(srv, verifier, allow, st.Ping, devNodePath, nodeHandler)

	addr := cfg.Listen
	log.Printf("cp public listener on %s", addr)
	h2Srv := &http2.Server{}
	h2keepalive.ConfigureServer(h2Srv)
	httpSrv := &http.Server{Addr: addr, Handler: h2c.NewHandler(publicHandler, h2Srv)}
	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	grace := cfg.ShutdownGrace
	select {
	case err := <-serveErr:
		log.Fatalf("cp: listener: %v", err)
	case <-ctx.Done():
		stop() // restore default signal handler so a second signal force-quits
		sdCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := srv.Shutdown(sdCtx); err != nil {
			log.Printf("cp: drain Attach streams: %v", err)
		}
		if internalSrv != nil {
			if err := internalSrv.Shutdown(sdCtx); err != nil {
				log.Printf("cp: drain internal TLS HTTP: %v", err)
			}
		}
		if err := httpSrv.Shutdown(sdCtx); err != nil {
			log.Printf("cp: drain HTTP: %v", err)
		}
	}
}

type internalRuntime struct {
	verifier    *mtls.PeerVerifier
	tlsConfig   *tls.Config
	client      *http.Client
	revocations *pki.RevocationState
}

func loadInternalRuntime(cfg CP, now func() time.Time) (*internalRuntime, error) {
	if cfg.Internal.Listen == "" {
		return nil, nil
	}
	rootPEM, err := os.ReadFile(cfg.Internal.RootCA)
	if err != nil {
		return nil, fmt.Errorf("read root CA: %w", err)
	}
	root, err := parseSingleRootCertificate(rootPEM)
	if err != nil {
		return nil, fmt.Errorf("parse root CA: %w", err)
	}
	if cfg.Auth.RootCA != "" {
		artifactRootPEM, readErr := os.ReadFile(cfg.Auth.RootCA)
		if readErr != nil {
			return nil, fmt.Errorf("read artifact root CA: %w", readErr)
		}
		artifactRoot, parseErr := parseSingleRootCertificate(artifactRootPEM)
		if parseErr != nil {
			return nil, fmt.Errorf("parse artifact root CA: %w", parseErr)
		}
		if !bytes.Equal(root.Raw, artifactRoot.Raw) {
			return nil, errors.New("internal TLS and auth artifacts must share the environment root")
		}
	}
	certPEM, err := os.ReadFile(cfg.Internal.Cert)
	if err != nil {
		return nil, fmt.Errorf("read CP certificate: %w", err)
	}
	chainPEM, err := os.ReadFile(cfg.Internal.Chain)
	if err != nil {
		return nil, fmt.Errorf("read CP certificate chain: %w", err)
	}
	keyPEM, err := os.ReadFile(cfg.Internal.Key)
	if err != nil {
		return nil, fmt.Errorf("read CP private key: %w", err)
	}
	identity, err := tls.X509KeyPair(append(certPEM, chainPEM...), keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load CP identity: %w", err)
	}

	issuers := make([]*x509.Certificate, 0, len(cfg.Internal.RevocationIssuers))
	issuerRoots := x509.NewCertPool()
	issuerRoots.AddCert(root)
	for _, path := range cfg.Internal.RevocationIssuers {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read revocation issuer %s: %w", path, readErr)
		}
		issuer, parseErr := pki.ParseCertPEM(raw)
		if parseErr != nil {
			return nil, fmt.Errorf("parse revocation issuer %s: %w", path, parseErr)
		}
		chains, verifyErr := issuer.Verify(x509.VerifyOptions{Roots: issuerRoots, CurrentTime: now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		if verifyErr != nil {
			return nil, fmt.Errorf("verify revocation issuer %s against environment root: %w", path, verifyErr)
		}
		if len(chains) == 0 || len(chains[0]) != 2 || !bytes.Equal(chains[0][1].Raw, root.Raw) {
			return nil, fmt.Errorf("verify revocation issuer %s: chain is not directly rooted in the environment root", path)
		}
		issuers = append(issuers, issuer)
	}
	state, err := pki.OpenRevocationState(cfg.Internal.RevocationState, issuers, now)
	if err != nil {
		return nil, fmt.Errorf("open certificate revocation state: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = state.Close()
		}
	}()
	if err := applyCertificateRevocations(state, cfg.Internal.RevocationCRLs); err != nil {
		return nil, err
	}
	for _, issuer := range issuers {
		if _, ok := state.HighestNumber(issuer.SerialNumber); !ok {
			return nil, fmt.Errorf("certificate revocation state has no current CRL for issuer %s", issuer.SerialNumber.Text(16))
		}
	}
	identityLeaf, err := x509.ParseCertificate(identity.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse CP identity leaf: %w", err)
	}
	identityChain := make([]*x509.Certificate, 0, len(identity.Certificate)-1)
	for _, raw := range identity.Certificate[1:] {
		cert, parseErr := x509.ParseCertificate(raw)
		if parseErr != nil {
			return nil, fmt.Errorf("parse CP identity chain: %w", parseErr)
		}
		identityChain = append(identityChain, cert)
	}
	principal, err := pki.VerifyPrincipal(identityLeaf, identityChain, pki.VerifyOptions{
		Root: root, TrustDomain: cfg.Internal.TrustDomain, CurrentTime: now(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsRevoked: state.IsRevoked,
	})
	if err != nil {
		return nil, fmt.Errorf("verify CP service identity: %w", err)
	}
	if principal.Kind != pki.KindService || principal.Role != pki.RoleCP {
		return nil, fmt.Errorf("CP identity is %s:%s, want service:%s", principal.Kind, principal.Role, pki.RoleCP)
	}
	verifier, err := mtls.NewPeerVerifier(mtls.PeerVerifierOptions{Root: root, TrustDomain: cfg.Internal.TrustDomain, CurrentTime: now, IsRevoked: state.IsRevoked})
	if err != nil {
		return nil, err
	}
	serverTLS, err := mtls.ServerConfig(mtls.ServerOptions{Verifier: verifier, Identity: identity, ClientMode: mtls.RequireClientCertificate})
	if err != nil {
		return nil, err
	}
	var client *http.Client
	if cfg.Internal.ServerName != "" {
		clientTLS, clientErr := mtls.ClientConfig(mtls.ClientOptions{Root: root, TrustDomain: cfg.Internal.TrustDomain, Identity: identity, ServerName: cfg.Internal.ServerName, ExpectedServiceRole: pki.RoleAuthService, CurrentTime: now, IsRevoked: state.IsRevoked})
		if clientErr != nil {
			return nil, clientErr
		}
		client = &http.Client{
			Transport:     &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: true},
			CheckRedirect: checkInternalRedirect,
		}
	}
	closeOnError = false
	return &internalRuntime{verifier: verifier, tlsConfig: serverTLS, client: client, revocations: state}, nil
}

func checkInternalRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return errors.New("internal redirect has no originating request")
	}
	origin := via[0].URL
	if !strings.EqualFold(req.URL.Scheme, "https") || req.URL.User != nil {
		return errors.New("internal redirect requires https without userinfo")
	}
	if !strings.EqualFold(origin.Scheme, req.URL.Scheme) || !strings.EqualFold(origin.Host, req.URL.Host) {
		return fmt.Errorf("internal redirect crosses origin from %s://%s to %s://%s", origin.Scheme, origin.Host, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

func applyCertificateRevocations(state *pki.RevocationState, paths []string) error {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read certificate CRL %s: %w", path, err)
		}
		if err := state.ApplyPEM(raw); err != nil {
			return fmt.Errorf("apply certificate CRL %s: %w", path, err)
		}
	}
	return nil
}

func refreshCertificateRevocations(ctx context.Context, state *pki.RevocationState, paths []string, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := applyCertificateRevocations(state, paths); err != nil {
				log.Printf("cp: refresh certificate revocations: %v", err)
			}
		}
	}
}

func buildPublicHandler(srv *cp.Server, verifier *auth.Verifier, allow weborigin.Allowlist, ready func(context.Context) error, devNodePath string, devNodeHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	spawnPath, spawnHandler := cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(
		rpclog.CorrelationInterceptor(), metrics.RPCInterceptor(), rpclog.RecoverInterceptor("cp"),
		rpclog.Interceptor("cp"), publicAuthInterceptor(verifier),
	))
	mux.Handle(spawnPath, spawnHandler)
	mux.HandleFunc("/ws/session", srv.HandleWS(verifier, allow))
	mux.Handle("/metrics", metrics.Handler())
	health.Register(mux, ready)
	mountInsecureDevNodeRoute(mux, devNodePath != "", devNodePath, devNodeHandler)
	return allow.CORS(mux)
}

func mountInsecureDevNodeRoute(mux *http.ServeMux, enabled bool, nodePath string, nodeHandler http.Handler) {
	if enabled && nodePath != "" && nodeHandler != nil {
		mux.Handle(nodePath, nodeHandler)
	}
}

func buildInternalHandler(verifier *mtls.PeerVerifier, spawnHandler, nodeHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/"+cpv1connect.SpawnServiceName+"/", spawnHandler)
	mux.Handle(nodev1connect.NodeServiceAttachProcedure, nodeHandler)
	policy := mtls.Policy{
		"service:authsvc": {
			cpv1connect.SpawnServiceAuthorizeGitHubMintProcedure:      {},
			cpv1connect.SpawnServiceSignalGitHubTokenRotatedProcedure: {},
		},
		"node:cloud":       {nodev1connect.NodeServiceAttachProcedure: {}},
		"node:self-hosted": {nodev1connect.NodeServiceAttachProcedure: {}},
	}
	bridge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal, ok := mtls.PrincipalFromContext(r.Context()); ok && principal.Kind == pki.KindNode {
			ctx := nodeauth.WithIdentity(r.Context(), principal)
			ctx = nodeauth.WithCertChain(ctx, peerCertChainPEM(r.TLS))
			r = r.WithContext(ctx)
		}
		mux.ServeHTTP(w, r)
	})
	return mtls.PrincipalMiddleware(verifier, policy.HTTPMiddleware(func(r *http.Request) string { return r.URL.Path }, bridge))
}

func peerCertChainPEM(state *tls.ConnectionState) []byte {
	if state == nil {
		return nil
	}
	var out []byte
	for _, cert := range state.PeerCertificates {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	return out
}

func buildInternalTLSServer(addr string, tlsConfig *tls.Config, handler http.Handler) (*http.Server, error) {
	if tlsConfig == nil {
		return nil, errors.New("internal TLS config is required")
	}
	server := &http.Server{Addr: addr, Handler: handler, TLSConfig: tlsConfig}
	h2Server := &http2.Server{}
	h2keepalive.ConfigureServer(h2Server)
	if err := http2.ConfigureServer(server, h2Server); err != nil {
		return nil, fmt.Errorf("configure internal HTTP/2: %w", err)
	}
	return server, nil
}

type publicAuth struct{ fallback connect.Interceptor }

func publicAuthInterceptor(verifier *auth.Verifier) connect.Interceptor {
	return publicAuth{fallback: verifier.Interceptor()}
}

func (i publicAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	userNext := i.fallback.WrapUnary(next)
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		switch req.Spec().Procedure {
		case cpv1connect.SpawnServiceAuthorizeGitHubMintProcedure, cpv1connect.SpawnServiceSignalGitHubTokenRotatedProcedure:
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("internal procedure"))
		default:
			return userNext(ctx, req)
		}
	}
}
func (i publicAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return i.fallback.WrapStreamingHandler(next)
}
func (publicAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func parseSingleRootCertificate(data []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("expected exactly one CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

func loadArtifactVerifier(cpCfg CP, now time.Time) (*token.Verifier, *token.SignerRevocationStore, error) {
	cfg := cpCfg.Auth
	if cfg.RootCA == "" && cfg.Environment == "" && cfg.SignerRevocationState == "" {
		return nil, nil, nil
	}
	rootPEM, err := os.ReadFile(cfg.RootCA)
	if err != nil {
		return nil, nil, fmt.Errorf("read root %s: %w", cfg.RootCA, err)
	}
	root, err := parseSingleRootCertificate(rootPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse root %s: %w", cfg.RootCA, err)
	}
	store, err := token.OpenSignerRevocationStore(cfg.SignerRevocationState, root, cfg.Environment, now)
	if err != nil {
		return nil, nil, fmt.Errorf("open signer revocation state %s: %w", cfg.SignerRevocationState, err)
	}
	if cfg.SignerRevocationStatement != "" {
		if err := store.LoadAndApply(cfg.SignerRevocationStatement, now); err != nil {
			_ = store.Close()
			return nil, nil, fmt.Errorf("apply signer revocation statement %s: %w", cfg.SignerRevocationStatement, err)
		}
	}
	verifier, err := token.NewVerifier(root, cfg.Environment, store)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	return verifier, store, nil
}

func parseTokens(s string) map[string]string {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if kv := strings.SplitN(pair, "=", 2); len(kv) == 2 {
			m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return m
}

func dir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

func splitTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
