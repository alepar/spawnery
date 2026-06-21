package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"spawnery/gen/auth/v1/authv1connect"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/h2keepalive"
	"spawnery/internal/health"
	applog "spawnery/internal/log"
	"spawnery/internal/metrics"
	"spawnery/internal/node"
	"spawnery/internal/node/nodeid"
	"spawnery/internal/pki"
	"spawnery/internal/rpclog"
	"spawnery/internal/runtime"
	"spawnery/internal/runtime/cri"
	"spawnery/internal/secrets/subkey"
	"spawnery/internal/spawnlet"
	"spawnery/internal/spawnlet/firewall"
	"spawnery/internal/storage/journal"
)

func main() {
	applog.Init(os.Getenv)
	cfg := spawnlet.ManagerConfig{
		AgentImage:    env("AGENT_IMAGE", "spawnery/stubagent:dev"),
		SidecarImage:  env("SIDECAR_IMAGE", "spawnery/sidecar:dev"),
		OpenRouterKey: os.Getenv("OPENROUTER_API_KEY"),
		DataRoot:      env("DATA_ROOT", "/var/lib/spawnlet/spawns"),

		NodeID:              env("NODE_ID", "node-1"),
		NodeClass:           env("NODE_CLASS", "cloud"),
		EgressEnforce:       getenvBool("EGRESS_ENFORCE", true),
		EgressAllowCIDRs:    splitCSV(os.Getenv("EGRESS_ALLOW_CIDRS")),
		// EgressFloorForceOff: DEV-ONLY override — disables the egress floor even for cloud
		// nodes where it is otherwise non-negotiable. MUST NOT be set in production.
		EgressFloorForceOff: getenvBool("EGRESS_FLOOR_FORCE_OFF", false),

		MemLimitMB:       getenvInt64("MEM_LIMIT_MB", 1024),
		CPULimit:         getenvFloat("CPU_LIMIT", 1.0),
		PidsLimit:        getenvInt64("PIDS_LIMIT", 256),
		ContainerRuntime: os.Getenv("CONTAINER_RUNTIME"),
		DeltaCapture:     getenvBool("DELTA_CAPTURE", false),
		// Delta tuning (spec §3/§7). Zero/empty → the manager applies its defaults
		// (squash depth 16; scrub /var/cache/apt,/var/lib/apt/lists,/tmp).
		// Quota thresholds are now evaluated CP-side (§6 transition-coordination-design):
		// set EVALUATOR_QUOTA_SUSPEND_MB on the CP, not here.
		DeltaSquashDepth: int(getenvInt64("DELTA_SQUASH_DEPTH", 0)),
		DeltaScrubPaths:  splitCSV(os.Getenv("DELTA_SCRUB_PATHS")),
		AdvertiseIP:      env("NODE_ADVERTISE_IP", "127.0.0.1"),
		UsernsMode:       env("USERNS_MODE", "off"),
	}
	mgr, err := buildManager(cfg)
	if err != nil {
		log.Fatalf("manager init: %v", err)
	}
	if err := configureJournal(mgr, cfg.DataRoot); err != nil {
		log.Fatalf("journal init: %v", err)
	}
	// SIGTERM/SIGINT cancels ctx; the node's serve loop returns and we gracefully reap our pods
	// (graceful teardown on shutdown complements reap-on-startup — see sp-8hf).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mgr.PreflightRuntime(ctx); err != nil {
		log.Fatalf("container runtime preflight failed: %v", err)
	}
	if cpURL := os.Getenv("CP_ADDR"); cpURL != "" {
		// CP-attached mode: dial the CP, no inbound listener.
		cfg := node.Config{
			NodeID:        env("NODE_ID", "node-1"),
			CPURL:         cpURL,
			MaxSpawns:     4,
			AgentImage:    env("AGENT_IMAGE", "spawnery/stubagent:dev"),
			AgentBinaries: splitCSV(os.Getenv("AGENT_BINARIES")),
			NodeClass:     env("NODE_CLASS", "cloud"),
			NodeOwner:     env("NODE_OWNER", ""),
		}
		// Terminal control plane (around CP for now): a small inbound listener so `spawnctl tmux`
		// can ask this node to start a mosh-backed terminal session for a spawn. The mosh UDP data
		// plane goes straight to this node. (CP-routed terminal control is sp-wsu.2.)
		if taddr := env("NODE_TERMINAL_ADDR", "127.0.0.1:9092"); taddr != "" {
			// Bind synchronously and FAIL FAST: a port-in-use here almost always means another
			// spawnlet is already running. Two nodes with the same NODE_ID corrupt the CP's routing
			// (it keys nodes by id) and flip spawns to UNREACHABLE — so refuse to start a duplicate.
			ln, err := net.Listen("tcp", taddr)
			if err != nil {
				log.Fatalf("terminal port %s is already in use — another spawnlet is likely running; "+
					"stop it first (e.g. `pkill -f bin/spawnlet`) so this node doesn't duplicate id %q "+
					"and corrupt CP routing: %v", taddr, cfg.NodeID, err)
			}
			tsrv := spawnlet.NewServer(mgr)
			tmux := http.NewServeMux()
			tmux.HandleFunc("/terminal", tsrv.HandleTerminal)
			tmux.HandleFunc("/exec", tsrv.HandleExec)
			health.Register(tmux, mgr.Ping)
			log.Printf("spawnlet terminal endpoint on %s (spawnctl attach -addr http://%s)", taddr, taddr)
			go func() {
				if err := http.Serve(ln, tmux); err != nil {
					log.Printf("terminal listener: %v", err)
				}
			}()
		}
		// Node-auth mode (sp-ova). insecure: h2c to CP_ADDR. enforced: mTLS to the CP node listener
		// presenting the enrolled cert (loaded from disk, or enrolled on first boot via the AS).
		httpc, dialURL, err := nodeCPClient(cpURL, cfg.NodeID)
		if err != nil {
			log.Fatalf("node: identity/transport setup: %v", err)
		}
		cfg.CPURL = dialURL
		cfg.NodeRootPEM = nodeRootPEM()
		// Owner-sealed secrets (sp-2ckv.4): in enforced mode build the HPKE sub-key holder signed by the
		// node's cert key, so the node can publish a sub-key and unseal delivered secrets. Best-effort:
		// insecure mode (no cert) and a key-parse failure both leave SubKeys nil (no sub-key published).
		if sk := nodeSubKeys(cfg.NodeID); sk != nil {
			cfg.SubKeys = sk
		}
		cfg.Verifier = buildIntentVerifier(cfg.NodeID, cfg.NodeOwner)
		cfg.GitHubMint = nodeGitHubMint()
		log.Printf("spawnlet attaching to CP at %s as %s", cfg.CPURL, cfg.NodeID)
		err = node.Run(ctx, mgr, httpc, cfg) // returns when ctx is cancelled (signal) or on fatal error
		gracefulStopAll(mgr)
		if err != nil && ctx.Err() == nil {
			log.Fatalf("node: %v", err)
		}
		return
	}

	// Standalone mode (unchanged): inbound spawn.v1 server + /ws.
	srv := spawnlet.NewServer(mgr)
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(metrics.RPCInterceptor(), rpclog.Interceptor("node"))))
	mux.HandleFunc("/ws/session", srv.HandleWS)
	mux.HandleFunc("/terminal", srv.HandleTerminal)
	mux.HandleFunc("/exec", srv.HandleExec)
	mux.Handle("/metrics", metrics.Handler())
	health.Register(mux, mgr.Ping)
	addr := env("SPAWNLET_ADDR", "127.0.0.1:9090")
	log.Printf("spawnlet listening on %s", addr)
	spawnletH2Srv := &http2.Server{}
	h2keepalive.ConfigureServer(spawnletH2Srv)
	log.Fatal(http.ListenAndServe(addr, h2c.NewHandler(mux, spawnletH2Srv)))
}

// gracefulStopAll tears down every spawn this node still runs, on a fresh (signal-independent) context
// with a bounded deadline so a slow runtime can't hang shutdown forever.
func gracefulStopAll(mgr *spawnlet.Manager) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if n := mgr.StopAll(shutdownCtx); n > 0 {
		log.Printf("graceful shutdown: stopped %d running spawn(s)", n)
	}
}

// buildManager selects the pod backend + egress floor by CONTAINER_RUNTIME: runsc -> a containerd
// CRI pod backend + the SPAWNLET-EGRESS floor; anything else -> the Docker backend + DOCKER-USER.
// configureJournal wires the transient-tier node-local journaler onto the
// manager when JOURNAL_BACKEND is set (filesystem|s3). It is OFF by default:
// with the env unset, journaling is disabled and every mount stays scratch-only
// (the guarded seam in the manager leaves existing behavior unchanged). Custody
// is node-local — the repo password is generated + sealed under a node key on
// this box; the CP never holds it (transient-tier §4).
func configureJournal(m *spawnlet.Manager, dataRoot string) error {
	kind := journal.BackendKind(os.Getenv("JOURNAL_BACKEND"))
	if kind == "" {
		return nil // journaling disabled (default)
	}
	root := env("JOURNAL_ROOT", filepath.Join(dataRoot, "journal"))

	var backend journal.BlobBackend
	var generationBackends journal.GenerationBackendProvider
	var gkm *journal.GenerationKeyManager
	switch kind {
	case journal.BackendFilesystem:
		var err error
		backend, err = journal.NewBackend(journal.BackendConfig{Kind: kind, FilesystemRoot: env("JOURNAL_FS_ROOT", filepath.Join(root, "blobs"))})
		if err != nil {
			return err
		}
	case journal.BackendS3:
		s3Endpoint := os.Getenv("JOURNAL_S3_ENDPOINT")
		if s3Endpoint == "" {
			return fmt.Errorf("journal s3: JOURNAL_S3_ENDPOINT is required")
		}
		adminEndpoint := os.Getenv("JOURNAL_GARAGE_ADMIN_ENDPOINT")
		adminToken := os.Getenv("JOURNAL_GARAGE_ADMIN_TOKEN")
		if adminEndpoint == "" || adminToken == "" {
			return fmt.Errorf("journal s3: JOURNAL_GARAGE_ADMIN_ENDPOINT and JOURNAL_GARAGE_ADMIN_TOKEN are required for generation-keyed journaling")
		}
		admin, err := journal.NewGarageAdmin(adminEndpoint, adminToken, &http.Client{Timeout: 15 * time.Second})
		if err != nil {
			return fmt.Errorf("journal genkey admin: %w", err)
		}
		gkm, err = journal.NewGenerationKeyManager(journal.GenerationKeyConfig{
			Admin:      admin,
			S3Endpoint: strings.TrimPrefix(strings.TrimPrefix(s3Endpoint, "https://"), "http://"),
			Region:     env("JOURNAL_S3_REGION", "garage"),
			DisableTLS: getenvBool("JOURNAL_S3_DISABLE_TLS", false),
		})
		if err != nil {
			return fmt.Errorf("journal genkey: %w", err)
		}
		generationBackends = gkm
	default:
		return fmt.Errorf("unknown JOURNAL_BACKEND %q (want filesystem|s3)", kind)
	}

	keyfile := env("JOURNAL_NODE_KEY", filepath.Join(root, "node.key"))
	if err := journal.GenerateNodeKeyfile(keyfile); err != nil {
		return fmt.Errorf("node key: %w", err)
	}
	custody, err := journal.NewNodeLocalCustody(keyfile, filepath.Join(root, "sealed"))
	if err != nil {
		return err
	}

	// Owner-sealed receiving custody (transient-tier §4, sp-u53.5.4): holds repo
	// passwords DELIVERED to this node for a cross-node resume / migration. Empty
	// at rest — every key arrives over the secret-delivery path and lives only in
	// memory for the episode.
	ownerSealed := journal.NewOwnerSealedCustody()
	jm, err := journal.NewManager(journal.Config{
		RepoRoot:           filepath.Join(root, "repos"),
		Backend:            backend,
		GenerationBackends: generationBackends,
		Custody:            custody,
		OwnerSealed:        ownerSealed,
	})
	if err != nil {
		return err
	}
	m.SetJournal(jm, filepath.Join(root, "state"))
	if gkm != nil {
		m.SetGenerationKeyManager(gkm)
	}
	log.Printf("journal: journaler enabled (backend=%s, root=%s; node-local + owner-sealed custody)", kind, root)
	return nil
}

func buildManager(cfg spawnlet.ManagerConfig) (*spawnlet.Manager, error) {
	// Warn early on unrecognised USERNS_MODE values (e.g. 'Remap', 'enabled'): they silently
	// degrade to cap-drop=ALL, which is safe but almost certainly not the operator's intent.
	switch cfg.UsernsMode {
	case "remap", "native", "off", "":
		// known values
	default:
		log.Printf("WARNING: unrecognized USERNS_MODE=%q (want remap|native|off) — treating as off (cap-drop=ALL)", cfg.UsernsMode)
	}
	if cfg.ContainerRuntime == "runsc" {
		endpoint := env("CRI_ENDPOINT", "unix:///run/containerd/containerd.sock")
		client, err := cri.Dial(endpoint)
		if err != nil {
			return nil, err
		}
		backend := cri.NewCRIPodBackend(client, env("CRI_RUNTIME_HANDLER", "runsc"))
		// POD_DNS (comma-separated) overrides the pod resolv.conf. Needed on hosts where /etc/resolv.conf
		// is the systemd-resolved 127.0.0.53 stub (unreachable from inside the pod); without a kubelet
		// the node must supply pod DNS itself.
		if v := os.Getenv("POD_DNS"); v != "" {
			for _, s := range strings.Split(v, ",") {
				if s = strings.TrimSpace(s); s != "" {
					backend.DNSServers = append(backend.DNSServers, s)
				}
			}
		}
		return spawnlet.NewManagerWithBackend(backend, firewall.NewCNIFloorApplier(), cfg), nil
	}
	rt, err := runtime.NewDocker()
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	// Userns-remap probe: when USERNS_MODE=remap, verify the daemon actually runs with userns-remap
	// and learn the remap base UID. On failure or missing userns → loud log + degraded fallback (off).
	// Node startup MUST NOT fail here; a misconfigured USERNS_MODE degrades to cap-drop=ALL.
	if cfg.UsernsMode == "remap" {
		base, active, perr := rt.UsernsRemap(context.Background())
		mode, remapBase := applyUsernsProbe(base, active, perr)
		switch {
		case perr != nil:
			log.Printf("USERNS_MODE=remap but daemon probe failed: %v — FALLING BACK TO DEGRADED (cap-drop=ALL)", perr)
		case !active:
			log.Printf("USERNS_MODE=remap but daemon reports no userns-remap (security options contain no name=userns) — FALLING BACK TO DEGRADED (cap-drop=ALL)")
		default:
			log.Printf("userns-remap active: base UID=%d (USERNS_MODE=remap confirmed)", remapBase)
		}
		cfg.UsernsMode = mode
		cfg.UsernsRemapBase = remapBase
	}
	return spawnlet.NewManager(rt, cfg), nil
}

// applyUsernsProbe converts the daemon probe result into the effective userns mode and remap
// base UID for the node config. It is pure (no logging, no mutation) so the degraded-fallback
// ordering is hermetically testable.
//
// The probe-error check is intentionally first: the case where the daemon reports userns active
// (active=true) but the base UID is unparseable (probeErr!=nil) degrades to "off" rather than
// proceeding with a zero remap base — a zero base would silently miscalculate host-side ownership
// for every userns-remapped mount.
func applyUsernsProbe(base uint32, active bool, probeErr error) (mode string, remapBase uint32) {
	if probeErr != nil {
		return "off", 0
	}
	if !active {
		return "off", 0
	}
	return "remap", base
}

// h2cClient mirrors cmd/spawnctl's: cleartext HTTP/2 for the CP dial.
// nodeCPClient selects the node->CP transport by NODE_AUTH_MODE. insecure (default): the h2c client to
// CP_ADDR. enforced: load the node's mTLS identity from NODE_ID_DIR (or enroll once via AS_URL +
// ENROLL_TOKEN, pinning NODE_ROOT_CA), and return an mTLS client targeting CP_NODE_ADDR.
//
// Enrollment uses a fingerprint-bound token (owner-sealed-secrets design §5): the node generates and
// persists its keypair FIRST, then logs the key's SPKI fingerprint. The operator gives that fingerprint
// to the account owner, who mints a token bound to it (IssueBoundEnrollmentToken) over the pinned AS
// connection and hands it back as ENROLL_TOKEN. The node redeems with the SAME key, so a token a
// compromised CP observed cannot be redeemed with a substituted key.
func nodeCPClient(insecureURL, nodeID string) (*http.Client, string, error) {
	if env("NODE_AUTH_MODE", "insecure") != "enforced" {
		return h2cClient(), insecureURL, nil
	}
	dir := env("NODE_ID_DIR", "/var/lib/spawnlet/identity")
	id, err := nodeid.Load(dir)
	if err != nil {
		// Generate/persist the node key up front so its fingerprint is stable across runs.
		key, kerr := nodeid.LoadOrGenerateKey(dir)
		if kerr != nil {
			return nil, "", fmt.Errorf("enforced mode: prepare node key: %w", kerr)
		}
		fp, ferr := pki.PublicKeyFingerprint(key.Public())
		if ferr != nil {
			return nil, "", fmt.Errorf("enforced mode: node key fingerprint: %w", ferr)
		}
		asURL, token := os.Getenv("AS_URL"), os.Getenv("ENROLL_TOKEN")
		if asURL == "" || token == "" {
			log.Printf("spawnlet node public-key fingerprint: %s", fp)
			log.Printf("spawnlet: mint a fingerprint-bound enrollment token for the above fingerprint, then set AS_URL + ENROLL_TOKEN")
			return nil, "", fmt.Errorf("enforced mode: no identity in %s and AS_URL/ENROLL_TOKEN unset: %w", dir, err)
		}
		rootPEM, rerr := os.ReadFile(env("NODE_ROOT_CA", filepath.Join(dir, "root.pem")))
		if rerr != nil {
			return nil, "", fmt.Errorf("enforced mode: pinned NODE_ROOT_CA required for enrollment: %w", rerr)
		}
		res, eerr := authsvc.RunEnrollWithKey(context.Background(), asURL, token, nodeID, key)
		if eerr != nil {
			return nil, "", fmt.Errorf("enroll: %w", eerr)
		}
		id = nodeid.Identity{CertPEM: res.CertPEM, ChainPEM: res.ChainPEM, KeyPEM: res.KeyPEM, RootPEM: rootPEM}
		if serr := nodeid.Save(dir, id); serr != nil {
			return nil, "", fmt.Errorf("persist identity: %w", serr)
		}
		log.Printf("spawnlet enrolled with AS %s (fingerprint %s); identity stored in %s", asURL, fp, dir)
	}
	client, err := id.MTLSClient()
	if err != nil {
		return nil, "", err
	}
	return client, env("CP_NODE_ADDR", "https://127.0.0.1:8081"), nil
}

// nodeSubKeys builds the node's HPKE sub-key holder from its enrolled cert key (sp-2ckv.4 §1), so the
// node can publish a cert-signed sub-key and unseal owner-delivered secrets. Returns nil in insecure
// mode (no identity) or if the on-disk key cannot be loaded/parsed — the node then publishes no sub-key
// and rejects SecretDelivery. The sub-key is signed by the SAME key as the node leaf cert (the RFC 9345
// delegated-credential pattern), so a sealing client verifies it chains to the pinned roots.
func nodeSubKeys(nodeID string) *subkey.Node {
	if env("NODE_AUTH_MODE", "insecure") != "enforced" {
		return nil
	}
	dir := env("NODE_ID_DIR", "/var/lib/spawnlet/identity")
	id, err := nodeid.Load(dir)
	if err != nil {
		log.Printf("subkey: no identity in %s, publishing no HPKE sub-key: %v", dir, err)
		return nil
	}
	key, err := pki.ParseKeyPEM(id.KeyPEM)
	if err != nil {
		log.Printf("subkey: parse node key, publishing no HPKE sub-key: %v", err)
		return nil
	}
	return subkey.NewNode(key, nodeID, 0)
}

func nodeRootPEM() []byte {
	if env("NODE_AUTH_MODE", "insecure") != "enforced" {
		return nil
	}
	dir := env("NODE_ID_DIR", "/var/lib/spawnlet/identity")
	path := os.Getenv("NODE_ROOT_CA")
	if path == "" {
		path = filepath.Join(dir, "root.pem")
	}
	rootPEM, err := os.ReadFile(path)
	if err != nil {
		log.Printf("node root PEM unavailable at %s; cross-node fork transfer will fail closed: %v", path, err)
		return nil
	}
	return rootPEM
}

// nodeGitHubMint builds the AS AuthService client for proactive GitHub access-token refresh
// (design §16.4). Returns nil when mint is disabled — proactive refresh is then off (spawns run
// on their delivered token until it lapses).
//
// Two paths:
//  1. D3 dev-github lane: NODE_GITHUB_MINT_DEV_NODE_ID set → plain HTTP h2c client with the
//     dev header identity. Works in any NODE_AUTH_MODE (no mTLS required). DEV-ONLY.
//  2. Enforced/prod lane: NODE_AUTH_MODE=enforced + AS_URL + loaded mTLS identity → mTLS client.
func nodeGitHubMint() node.GitHubMintClient {
	asURL := os.Getenv("AS_URL")
	// D3 dev-github lane: relaxed node->AS over plain HTTP with a header identity (NOT mTLS). The
	// secure mTLS leg is proven by TestGitHubE2E_* and is the enforced/prod path below.
	if devNodeID := strings.TrimSpace(os.Getenv("NODE_GITHUB_MINT_DEV_NODE_ID")); devNodeID != "" {
		if asURL == "" {
			log.Printf("github mint: NODE_GITHUB_MINT_DEV_NODE_ID set but AS_URL empty — relaxed mint disabled")
			return nil
		}
		log.Printf("github mint: DEV RELAXED node->AS (plain HTTP, header identity %q) — NOT for production", devNodeID)
		return authv1connect.NewAuthServiceClient(h2cClient(), asURL,
			connect.WithInterceptors(devNodeIDInterceptor{nodeID: devNodeID}))
	}
	if env("NODE_AUTH_MODE", "insecure") != "enforced" {
		return nil
	}
	if asURL == "" {
		return nil
	}
	dir := env("NODE_ID_DIR", "/var/lib/spawnlet/identity")
	id, err := nodeid.Load(dir)
	if err != nil {
		log.Printf("github refresh disabled: no identity in %s: %v", dir, err)
		return nil
	}
	client, err := id.MTLSClient()
	if err != nil {
		log.Printf("github refresh disabled: mTLS client: %v", err)
		return nil
	}
	return authv1connect.NewAuthServiceClient(client, asURL)
}

// devNodeIDInterceptor injects the D3 dev relaxed node-identity header on every call to the AS.
// DEV-ONLY — used solely by the dev-github lane (NODE_GITHUB_MINT_DEV_NODE_ID); NOT for production.
type devNodeIDInterceptor struct{ nodeID string }

func (i devNodeIDInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("X-Spawnery-Dev-Node-Id", i.nodeID)
		return next(ctx, req)
	}
}
func (i devNodeIDInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("X-Spawnery-Dev-Node-Id", i.nodeID)
		return conn
	}
}
func (i devNodeIDInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// buildIntentVerifier builds the A4 IntentVerifier from the environment [AC1][AM12].
//
// NODE_AUTH_MODE=insecure (default): AuthModeVerifyLog — the full verification chain runs but
// failures are logged rather than enforced. This satisfies AM12 (dev/prod parity via verify-and-log).
// NODE_AUTH_MODE=enforced: AuthModeEnforced — failures block execution and return NACK codes.
//
// NODE_AS_PUBKEYS (comma-separated PEM file paths): the AS session signing public keys the node
// uses to verify the aud=node access token. In insecure mode an empty key set is valid (the token
// step fails with ErrUnknownKey which is logged but not enforced). In enforced mode, the AS public
// keys must be configured here.
//
// NODE_OWNER: if non-empty AND NODE_AUTH_MODE=enforced, enables the self-hosted owner check
// (the token's account_id must equal NODE_OWNER).
func buildIntentVerifier(nodeID, nodeOwner string) *node.IntentVerifier {
	enforced := env("NODE_AUTH_MODE", "insecure") == "enforced"
	authMode := node.AuthModeVerifyLog
	if enforced {
		authMode = node.AuthModeEnforced
	}

	ks, err := loadNodeKeySet(env("NODE_AS_PUBKEYS", ""))
	if err != nil {
		log.Printf("buildIntentVerifier: load AS pubkeys: %v (verification will log token failures)", err)
	} else if len(ks) > 0 {
		log.Printf("node: loaded %d AS pubkey(s) for intent verification", len(ks))
	}

	selfHosted := enforced && nodeOwner != ""
	return node.NewIntentVerifier(ks, nodeOwner, nodeID, selfHosted, authMode, nil)
}

// loadNodeKeySet parses comma-separated PEM file paths into a token.KeySet.
// Empty s returns an empty KeySet (valid in insecure mode — token step logs ErrUnknownKey).
func loadNodeKeySet(s string) (token.KeySet, error) {
	if s == "" {
		return token.KeySet{}, nil
	}
	var pubs []ed25519.PublicKey
	for _, p := range splitCSV(s) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
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

func h2cClient() *http.Client {
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	h2keepalive.ConfigureTransport(tr)
	return &http.Client{Transport: tr}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "TRUE"
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func getenvInt64(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getenvFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
