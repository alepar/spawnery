package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
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
	// auth.mode: "dev" (default) | "prod". Default is "dev" — a misconfigured prod is permissive.
	// Prod REQUIRES auth.as_session_pubkeys; dev tokens are ignored in prod.
	devMode := cfg.DevMode()
	if !devMode {
		log.Printf("cp: auth mode=prod (dev tokens ignored)")
	} else {
		log.Printf("cp: auth mode=dev (dev tokens active; NOT FOR PRODUCTION)")
	}

	// --- AS session pubkeys (auth.as_session_pubkeys = comma-separated PEM file paths) ---
	ks, err := loadKeySet(cfg.Auth.ASSessionPubkeys)
	if err != nil {
		log.Fatalf("cp: load AS pubkeys: %v", err)
	}
	if len(ks) > 0 {
		log.Printf("cp: loaded %d AS session pubkey(s)", len(ks))
	}
	if !devMode && len(ks) == 0 {
		log.Fatalf("cp: auth.mode=prod requires auth.as_session_pubkeys (no keys loaded)")
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
		Keys:      ks,
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
		var devASPriv ed25519.PrivateKey
		var devASKeyID string
		var devASErr error
		if p := cfg.Auth.DevASKey; p != "" {
			var pemBytes []byte
			pemBytes, devASErr = os.ReadFile(p)
			if devASErr == nil {
				devASPriv, devASKeyID, devASErr = token.LoadSigningKey(pemBytes)
			}
			if devASErr != nil {
				log.Fatalf("cp: load auth.dev_as_key: %v", devASErr)
			}
			log.Printf("cp: loaded dev AS key from %s (id=%s) [AM12]", p, devASKeyID)
		} else {
			_, devASPriv, devASErr = ed25519.GenerateKey(rand.Reader)
			if devASErr != nil {
				log.Fatalf("cp: generate ephemeral dev AS key: %v", devASErr)
			}
			devASKeyID, devASErr = token.KeyID(devASPriv.Public().(ed25519.PublicKey))
			if devASErr != nil {
				log.Fatalf("cp: derive dev AS key id: %v", devASErr)
			}
			log.Printf("cp: using ephemeral dev AS key (id=%s) [AM12]", devASKeyID)
		}
		srv.SetDevASKey(devASPriv, devASKeyID)
		// auth.dev_intent_enabled: opt into the two-phase sign flow in dev mode.
		if cfg.Auth.DevIntentEnabled {
			srv.SetIntentEnabled(true)
			log.Printf("cp: dev intent flow enabled (auth.dev_intent_enabled=true) [AM12]")
		} else {
			log.Printf("cp: dev intent flow off (set auth.dev_intent_enabled=true to enable; web spawns proceed without signing) [AM12]")
		}
	}

	// CP→AS GitHub link-status preflight: gated on CP_AS_URL; also uses the existing CP_AS_RPC_SECRET.
	// When set, CreateSpawn checks the owner's GitHub link state before persisting a github:-mount spawn.
	if asURL := strings.TrimSpace(cfg.Auth.ASURL); asURL != "" {
		srv.SetASLinkChecker(asURL, string(cfg.Auth.ASRPCSecret))
		log.Printf("cp: GitHub link preflight checker wired to AS %s", asURL)
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
		bearer := string(cfg.Auth.ASCPSecret)
		interval := cfg.Auth.RevocationPollInterval
		poller := auth.NewFeedPoller(http.DefaultClient, feedURL, bearer, ks, revreg, interval)
		safego.Go("cp.revocation-poller", func() { poller.Run(ctx) })
		log.Printf("cp: revocation feed poller started (url=%s interval=%s)", feedURL, interval)
	}

	mux := http.NewServeMux()
	cpAuthInterceptor := verifier.Interceptor()
	if asSecret := string(cfg.Auth.ASRPCSecret); asSecret != "" {
		cpAuthInterceptor = auth.NewServiceScopedInterceptor(
			verifier,
			auth.ServiceSecretHeader,
			asSecret,
			cpv1connect.SpawnServiceAuthorizeGitHubMintProcedure,
			cpv1connect.SpawnServiceSignalGitHubTokenRotatedProcedure,
		)
		log.Printf("cp: AS GitHub coordination RPC secret enabled")
	}
	mux.Handle(cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(rpclog.CorrelationInterceptor(), metrics.RPCInterceptor(), rpclog.RecoverInterceptor("cp"), rpclog.Interceptor("cp"), cpAuthInterceptor)))
	mux.HandleFunc("/ws/session", srv.HandleWS(verifier, allow))
	mux.Handle("/metrics", metrics.Handler())
	health.Register(mux, st.Ping)

	// Node-auth mode (sp-ova). insecure (dev/test default): nodes share the main h2c listener with no
	// auth — identity falls back to the self-asserted Register fields. enforced: nodes connect over mTLS
	// on a dedicated listener and their identity is the verified client cert (see internal/cp/nodeauth).
	mode := nodeauth.Mode(cfg.Node.AuthMode)
	nodePath, nodeHandler := nodev1connect.NewNodeServiceHandler(srv, connect.WithInterceptors(rpclog.CorrelationInterceptor(), metrics.RPCInterceptor(), rpclog.RecoverInterceptor("cp"), rpclog.Interceptor("cp")))
	var nodeTLSSrv *http.Server // non-nil only in enforced mode; shut down alongside httpSrv
	if mode == nodeauth.ModeEnforced {
		var tlsErr error
		nodeTLSSrv, tlsErr = buildNodeTLSServer(cfg.Node.Listen, nodePath, nodeHandler, cfg.Node.RootCA, cfg.Node.TLSCert, cfg.Node.TLSKey)
		if tlsErr != nil {
			log.Fatalf("cp: build node mTLS listener: %v", tlsErr)
		}
		safego.Go("cp.node-mtls-listener", func() {
			if err := nodeTLSSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				log.Fatalf("cp: node mTLS listener: %v", err)
			}
		})
	} else {
		mux.Handle(nodePath, nodeHandler)
	}

	addr := cfg.Listen
	log.Printf("cp listening on %s (node-auth mode=%s)", addr, mode)
	h2Srv := &http2.Server{}
	h2keepalive.ConfigureServer(h2Srv)
	httpSrv := &http.Server{Addr: addr, Handler: h2c.NewHandler(allow.CORS(mux), h2Srv)}
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
		if nodeTLSSrv != nil {
			if err := nodeTLSSrv.Shutdown(sdCtx); err != nil {
				log.Printf("cp: drain node TLS HTTP: %v", err)
			}
		}
		if err := httpSrv.Shutdown(sdCtx); err != nil {
			log.Printf("cp: drain HTTP: %v", err)
		}
	}
}

// buildNodeTLSServer configures and returns the NodeService mTLS http.Server without starting it.
// The caller is responsible for calling ListenAndServeTLS and Shutdown.
func buildNodeTLSServer(addr, nodePath string, nodeHandler http.Handler, rootCAPath, certPath, keyPath string) (*http.Server, error) {
	rootPEM, err := os.ReadFile(rootCAPath)
	if err != nil {
		return nil, fmt.Errorf("read pinned root CA: %w", err)
	}
	root, err := pki.ParseCertPEM(rootPEM)
	if err != nil {
		return nil, fmt.Errorf("parse pinned root CA: %w", err)
	}
	serverCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load CP server cert: %w", err)
	}
	nodeMux := http.NewServeMux()
	nodeMux.Handle(nodePath, nodeHandler)
	server := &http.Server{
		Addr:    addr,
		Handler: nodeauth.Middleware(nodeauth.ModeEnforced, root, nodeMux),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientAuth:   tls.RequireAnyClientCert,
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"h2", "http/1.1"},
		},
	}
	nodeTLSH2Srv := &http2.Server{}
	h2keepalive.ConfigureServer(nodeTLSH2Srv)
	if err := http2.ConfigureServer(server, nodeTLSH2Srv); err != nil {
		return nil, fmt.Errorf("configure http2: %w", err)
	}
	log.Printf("cp: node mTLS listener on %s", addr)
	return server, nil
}

// loadKeySet parses comma-separated PEM file paths into an ordered token.KeySet.
// Empty s returns an empty set (valid in dev mode).
func loadKeySet(s string) (token.KeySet, error) {
	if s == "" {
		return token.KeySet{}, nil
	}
	var pubs []ed25519.PublicKey
	for _, p := range splitTrim(s, ",") {
		pemBytes, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		pub, err := token.ParsePublicKeyPEM(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		pubs = append(pubs, pub)
	}
	return token.NewKeySet(pubs...)
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
