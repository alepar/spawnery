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
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/cp"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/nodeauth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/pki"
	"spawnery/internal/weborigin"
)

const sqliteDefaultDSN = "file:cp.db?_pragma=busy_timeout(5000)"

func storeConfigFromEnv(get func(string) string) (store.Config, error) {
	driver := get("CP_STORE_DRIVER")
	if driver == "" {
		driver = "sqlite"
	}
	dsn := get("CP_STORE_DSN")
	if dsn == "" {
		dsn = sqliteDefaultDSN
	}
	if driver == "postgres" && (dsn == "" || dsn == sqliteDefaultDSN) {
		return store.Config{}, fmt.Errorf("CP_STORE_DRIVER=postgres requires CP_STORE_DSN (a postgres DSN)")
	}
	return store.Config{Driver: driver, DSN: dsn}, nil
}

func main() {
	reg := registry.New()
	rt := router.New()
	sched := scheduler.New(reg, rt, 60*time.Second)

	ctx := context.Background()

	// --- Auth mode ---
	// CP_AUTH_MODE: "dev" (default) | "prod".
	// IMPORTANT: default is "dev" — a misconfigured prod is permissive.
	// Prod REQUIRES CP_AS_SESSION_PUBKEYS; dev tokens are ignored in prod.
	authMode := env("CP_AUTH_MODE", "dev")
	devMode := authMode != "prod"
	if !devMode {
		log.Printf("cp: auth mode=prod (CP_DEV_TOKENS ignored)")
	} else {
		log.Printf("cp: auth mode=dev (CP_DEV_TOKENS active; NOT FOR PRODUCTION)")
	}

	// --- AS session pubkeys (CP_AS_SESSION_PUBKEYS = comma-separated PEM file paths) ---
	ks, err := loadKeySet(env("CP_AS_SESSION_PUBKEYS", ""))
	if err != nil {
		log.Fatalf("cp: load AS pubkeys: %v", err)
	}
	if len(ks) > 0 {
		log.Printf("cp: loaded %d AS session pubkey(s)", len(ks))
	}
	if !devMode && len(ks) == 0 {
		log.Fatalf("cp: CP_AUTH_MODE=prod requires CP_AS_SESSION_PUBKEYS (no keys loaded)")
	}

	// --- Revocation + session registries ---
	sessions := auth.NewSessionRegistry()
	revreg := auth.NewRevocationRegistry(sessions)

	// --- Dev tokens (honored only in dev mode) ---
	devTokens := map[string]string{}
	if devMode {
		devTokens = parseTokens(env("CP_DEV_TOKENS", "dev-token=dev"))
	}

	// --- Verifier ---
	verifier := auth.NewVerifier(auth.VerifierConfig{
		Keys:      ks,
		DevTokens: devTokens,
		DevMode:   devMode,
		Revoked:   revreg,
	})

	storeCfg, err := storeConfigFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("store config: %v", err)
	}
	st, err := store.Open(ctx, storeCfg)
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
	if p := env("CP_TELEMETRY", "telemetry/events.jsonl"); p != "" {
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
	srv.SetMaxSpawnsPerOwner(envInt("CP_MAX_SPAWNS_PER_OWNER", 5))
	srv.SetSessionRegistry(sessions)
	srv.SetVerify(verifier.Verify)
	srv.SetDevMode(devMode)
	if ri := envDuration("CP_SESSION_REAUTH_INTERVAL", 0); ri > 0 {
		srv.SetReauthInterval(ri)
	}

	// A4 intent flow setup [AC1][AM12].
	// Prod mode: intent flow always active; clients obtain node tokens from the real AS.
	// Dev mode: intent flow active when CP_DEV_AS_KEY is set (explicit) or by default (ephemeral key).
	//   In either case the CP mints cnf-bearing aud=node tokens in SubmitIntent so the full 8-step
	//   verification chain runs at the node under AuthModeVerifyLog (verify-and-log, not skip).
	if !devMode {
		srv.SetIntentEnabled(true)
	} else {
		var devASPriv ed25519.PrivateKey
		var devASKeyID string
		var devASErr error
		if p := env("CP_DEV_AS_KEY", ""); p != "" {
			var pemBytes []byte
			pemBytes, devASErr = os.ReadFile(p)
			if devASErr == nil {
				devASPriv, devASKeyID, devASErr = token.LoadSigningKey(pemBytes)
			}
			if devASErr != nil {
				log.Fatalf("cp: load CP_DEV_AS_KEY: %v", devASErr)
			}
			log.Printf("cp: loaded dev AS key from %s (id=%s) for cnf-bearing node token minting [AM12]", p, devASKeyID)
		} else {
			_, devASPriv, devASErr = ed25519.GenerateKey(rand.Reader)
			if devASErr != nil {
				log.Fatalf("cp: generate ephemeral dev AS key: %v", devASErr)
			}
			devASKeyID, devASErr = token.KeyID(devASPriv.Public().(ed25519.PublicKey))
			if devASErr != nil {
				log.Fatalf("cp: derive dev AS key id: %v", devASErr)
			}
			log.Printf("cp: using ephemeral dev AS key (id=%s) for cnf-bearing node token minting [AM12]", devASKeyID)
		}
		srv.SetDevASKey(devASPriv, devASKeyID)
		srv.SetIntentEnabled(true)
	}

	srv.StartReconciler(ctx) // background loop: drive model_applied=false spawns to convergence (sp-bp9w.7)

	// Browser-origin allowlist for CORS + the WS upgrade ([WM18]). Empty = dev mode
	// (localhost-only origins); production sets the exact canonical SPA origin(s).
	allow := weborigin.FromEnv(env("CP_ALLOWED_ORIGINS", ""))
	if allow.Dev() {
		log.Printf("cp: CP_ALLOWED_ORIGINS unset — dev mode, allowing localhost browser origins only")
	}

	// Start revocation feed poller if configured.
	if feedURL := env("CP_AS_REVOCATION_URL", ""); feedURL != "" {
		bearer := env("CP_AS_CP_SECRET", "")
		interval := envDuration("CP_REVOCATION_POLL_INTERVAL", 30*time.Second)
		poller := auth.NewFeedPoller(http.DefaultClient, feedURL, bearer, ks, revreg, interval)
		go poller.Run(ctx)
		log.Printf("cp: revocation feed poller started (url=%s interval=%s)", feedURL, interval)
	}

	mux := http.NewServeMux()
	mux.Handle(cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(verifier.Interceptor())))
	mux.HandleFunc("/ws/session", srv.HandleWS(verifier, allow))

	// Node-auth mode (sp-ova). insecure (dev/test default): nodes share the main h2c listener with no
	// auth — identity falls back to the self-asserted Register fields. enforced: nodes connect over mTLS
	// on a dedicated listener and their identity is the verified client cert (see internal/cp/nodeauth).
	mode := nodeauth.Mode(env("NODE_AUTH_MODE", string(nodeauth.ModeInsecure)))
	nodePath, nodeHandler := nodev1connect.NewNodeServiceHandler(srv)
	if mode == nodeauth.ModeEnforced {
		go func() {
			if err := serveNodeTLS(env("CP_NODE_LISTEN", "127.0.0.1:8081"), nodePath, nodeHandler); err != nil {
				log.Fatalf("cp: node mTLS listener: %v", err)
			}
		}()
	} else {
		mux.Handle(nodePath, nodeHandler)
	}

	addr := env("CP_LISTEN", "127.0.0.1:8080")
	log.Printf("cp listening on %s (node-auth mode=%s)", addr, mode)
	log.Fatal(http.ListenAndServe(addr, h2c.NewHandler(allow.CORS(mux), &http2.Server{})))
}

// serveNodeTLS runs the NodeService over mTLS on its own listener.
func serveNodeTLS(addr, nodePath string, nodeHandler http.Handler) error {
	rootPEM, err := os.ReadFile(env("CP_NODE_ROOT_CA", "/etc/spawnery/cp/node-root-ca.pem"))
	if err != nil {
		return fmt.Errorf("read pinned root CA: %w", err)
	}
	root, err := pki.ParseCertPEM(rootPEM)
	if err != nil {
		return fmt.Errorf("parse pinned root CA: %w", err)
	}
	serverCert, err := tls.LoadX509KeyPair(
		env("CP_NODE_TLS_CERT", "/etc/spawnery/cp/server.pem"),
		env("CP_NODE_TLS_KEY", "/etc/spawnery/cp/server-key.pem"),
	)
	if err != nil {
		return fmt.Errorf("load CP server cert: %w", err)
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
	if err := http2.ConfigureServer(server, &http2.Server{}); err != nil {
		return fmt.Errorf("configure http2: %w", err)
	}
	log.Printf("cp: node mTLS listener on %s", addr)
	return server.ListenAndServeTLS("", "")
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

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
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
