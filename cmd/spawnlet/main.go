package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"spawnery/gen/auth/v1/authv1connect"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/config"
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
	"spawnery/internal/storage"
	"spawnery/internal/storage/journal"
)

func main() {
	applog.Init(os.Getenv)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("spawnlet: config: %v", err)
	}

	managerCfg := spawnlet.ManagerConfig{
		AgentImage:          cfg.AgentImage,
		SidecarImage:        cfg.SidecarImage,
		OpenRouterKey:       string(cfg.OpenRouterKey),
		DataRoot:            cfg.DataRoot,
		NodeID:              cfg.Node.ID,
		NodeClass:           cfg.Node.Class,
		EgressEnforce:       cfg.Egress.Enforce,
		EgressAllowCIDRs:    cfg.Egress.AllowCIDRs,
		EgressFloorForceOff: cfg.Egress.FloorForceOff,
		MemLimitMB:          cfg.Limits.MemMB,
		CPULimit:            cfg.Limits.CPU,
		PidsLimit:           cfg.Limits.Pids,
		ContainerRuntime:    cfg.ContainerRuntime,
		DeltaCapture:        cfg.Delta.Capture,
		DeltaSquashDepth:    cfg.Delta.SquashDepth,
		DeltaScrubPaths:     cfg.Delta.ScrubPaths,
		AdvertiseIP:         cfg.Node.AdvertiseIP,
		UsernsMode:          cfg.UsernsMode,
	}
	if err := configureGitHubOverride(&managerCfg, cfg); err != nil {
		log.Fatalf("github override: %v", err)
	}
	mgr, err := buildManager(managerCfg, cfg.CRI.Endpoint, cfg.CRI.RuntimeHandler, cfg.PodDNS)
	if err != nil {
		log.Fatalf("manager init: %v", err)
	}
	if err := configureJournal(mgr, cfg); err != nil {
		log.Fatalf("journal init: %v", err)
	}
	// SIGTERM/SIGINT cancels ctx; the node's serve loop returns and we gracefully reap our pods
	// (graceful teardown on shutdown complements reap-on-startup — see sp-8hf).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mgr.PreflightRuntime(ctx); err != nil {
		log.Fatalf("container runtime preflight failed: %v", err)
	}
	if cfg.CP.Addr != "" {
		// CP-attached mode: dial the CP, no inbound listener.
		nodeCfg := node.Config{
			NodeID:          cfg.Node.ID,
			CPURL:           cfg.CP.Addr,
			MaxSpawns:       4,
			AgentImage:      cfg.AgentImage,
			AgentBinaries:   cfg.AgentBinaries,
			NodeClass:       cfg.Node.Class,
			NodeOwner:       cfg.Node.Owner,
			NodeTrustDomain: cfg.Node.TrustDomain,
		}
		// Terminal control plane (around CP for now): a small inbound listener so `spawnctl tmux`
		// can ask this node to start a mosh-backed terminal session for a spawn. The mosh UDP data
		// plane goes straight to this node. (CP-routed terminal control is sp-wsu.2.)
		if taddr := cfg.Node.TerminalAddr; taddr != "" {
			// Bind synchronously and FAIL FAST: a port-in-use here almost always means another
			// spawnlet is already running. Two nodes with the same NODE_ID corrupt the CP's routing
			// (it keys nodes by id) and flip spawns to UNREACHABLE — so refuse to start a duplicate.
			ln, err := net.Listen("tcp", taddr)
			if err != nil {
				log.Fatalf("terminal port %s is already in use — another spawnlet is likely running; "+
					"stop it first (e.g. `pkill -f bin/spawnlet`) so this node doesn't duplicate id %q "+
					"and corrupt CP routing: %v", taddr, cfg.Node.ID, err)
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
		httpc, dialURL, err := nodeCPClient(cfg, cfg.CP.Addr, cfg.Node.ID)
		if err != nil {
			log.Fatalf("node: identity/transport setup: %v", err)
		}
		nodeCfg.CPURL = dialURL
		nodeCfg.NodeRootPEM = nodeRootPEM(cfg)
		artifactTrust, err := loadArtifactVerifier(cfg, time.Now())
		if err != nil {
			if cfg.Node.AuthMode == "enforced" {
				log.Fatalf("node: artifact trust setup: %v", err)
			}
			log.Printf("node: artifact trust unavailable in insecure mode: %v", err)
		}
		if artifactTrust != nil {
			reloadCtx, cancelReload := context.WithCancel(ctx)
			reloadDone := artifactTrust.watch(reloadCtx, time.Second, time.Now, func(err error) {
				log.Printf("node: signer-revocation reload failed: %v", err)
			})
			defer func() {
				cancelReload()
				<-reloadDone
				closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancelClose()
				if err := artifactTrust.CloseContext(closeCtx); err != nil {
					log.Printf("node: artifact trust close: %v", err)
				}
			}()
		}
		// Owner-sealed secrets (sp-2ckv.4): in enforced mode build the HPKE sub-key holder signed by the
		// node's cert key, so the node can publish a sub-key and unseal delivered secrets. Best-effort:
		// insecure mode (no cert) and a key-parse failure both leave SubKeys nil (no sub-key published).
		if sk := nodeSubKeys(cfg, cfg.Node.ID); sk != nil {
			nodeCfg.SubKeys = sk
		}
		nodeCfg.Verifier, err = buildIntentVerifier(cfg, artifactTrust, cfg.Node.ID, cfg.Node.Owner)
		if err != nil {
			log.Fatalf("node: intent verifier setup: %v", err)
		}
		nodeCfg.GitHubMint = nodeGitHubMint(cfg)
		log.Printf("spawnlet attaching to CP at %s as %s", nodeCfg.CPURL, cfg.Node.ID)
		err = node.Run(ctx, mgr, httpc, nodeCfg) // returns when ctx is cancelled (signal) or on fatal error
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
	addr := cfg.SpawnletAddr
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

// buildManager selects the pod backend + egress floor by ContainerRuntime: "runsc" -> a containerd
// CRI pod backend + the SPAWNLET-EGRESS floor; anything else -> the Docker backend + DOCKER-USER.
// criEndpoint and criRtHandler are only used in the "runsc" lane; empty strings use lane defaults.
// podDNS overrides the pod resolv.conf in the CRI lane (nil/empty = inherit host resolver).
//
// configureJournal wires the transient-tier node-local journaler onto the
// manager when JOURNAL_BACKEND is set (filesystem|s3). It is OFF by default:
// with the env unset, journaling is disabled and every mount stays scratch-only
// (the guarded seam in the manager leaves existing behavior unchanged). Custody
// is node-local — the repo password is generated + sealed under a node key on
// this box; the CP never holds it (transient-tier §4).
//
// configureJournal is driven by the typed cfg.Journal config (populated by the YAML layers, the
// JOURNAL_* env aliases, and --set alike), not by reading the environment directly — so
// `--set journal.backend=...` and SPAWNERY_CONFIG_DIR overrides take effect.
func configureJournal(m *spawnlet.Manager, cfg *Spawnlet) error {
	j := cfg.Journal
	kind := journal.BackendKind(j.Backend)
	if kind == "" {
		return nil // journaling disabled (default)
	}
	root := j.Root
	if root == "" {
		root = filepath.Join(cfg.DataRoot, "journal")
	}

	var backend journal.BlobBackend
	var generationBackends journal.GenerationBackendProvider
	var gkm *journal.GenerationKeyManager
	switch kind {
	case journal.BackendFilesystem:
		var err error
		fsRoot := j.FSRoot
		if fsRoot == "" {
			fsRoot = filepath.Join(root, "blobs")
		}
		backend, err = journal.NewBackend(journal.BackendConfig{Kind: kind, FilesystemRoot: fsRoot})
		if err != nil {
			return err
		}
	case journal.BackendS3:
		if j.S3.Endpoint == "" {
			return fmt.Errorf("journal s3: journal.s3.endpoint is required")
		}
		if j.S3.GarageAdminEndpoint == "" || string(j.S3.GarageAdminToken) == "" {
			return fmt.Errorf("journal s3: journal.s3.garage_admin_endpoint and journal.s3.garage_admin_token are required for generation-keyed journaling")
		}
		admin, err := journal.NewGarageAdmin(j.S3.GarageAdminEndpoint, string(j.S3.GarageAdminToken), &http.Client{Timeout: 15 * time.Second})
		if err != nil {
			return fmt.Errorf("journal genkey admin: %w", err)
		}
		region := j.S3.Region
		if region == "" {
			region = "garage"
		}
		gkm, err = journal.NewGenerationKeyManager(journal.GenerationKeyConfig{
			Admin:      admin,
			S3Endpoint: strings.TrimPrefix(strings.TrimPrefix(j.S3.Endpoint, "https://"), "http://"),
			Region:     region,
			DisableTLS: j.S3.DisableTLS,
		})
		if err != nil {
			return fmt.Errorf("journal genkey: %w", err)
		}
		generationBackends = gkm
	default:
		return fmt.Errorf("unknown JOURNAL_BACKEND %q (want filesystem|s3)", kind)
	}

	keyfile := j.NodeKey
	if keyfile == "" {
		keyfile = filepath.Join(root, "node.key")
	}
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

func buildManager(cfg spawnlet.ManagerConfig, criEndpoint, criRtHandler string, podDNS []string) (*spawnlet.Manager, error) {
	// Warn early on unrecognised USERNS_MODE values (e.g. 'Remap', 'enabled'): they silently
	// degrade to cap-drop=ALL, which is safe but almost certainly not the operator's intent.
	switch cfg.UsernsMode {
	case "remap", "native", "off", "":
		// known values
	default:
		log.Printf("WARNING: unrecognized USERNS_MODE=%q (want remap|native|off) — treating as off (cap-drop=ALL)", cfg.UsernsMode)
	}
	if cfg.ContainerRuntime == "runsc" {
		if criEndpoint == "" {
			criEndpoint = "unix:///run/containerd/containerd.sock"
		}
		if criRtHandler == "" {
			criRtHandler = "runsc"
		}
		client, err := cri.Dial(criEndpoint)
		if err != nil {
			return nil, err
		}
		backend := cri.NewCRIPodBackend(client, criRtHandler)
		// podDNS overrides the pod resolv.conf. Needed on hosts where /etc/resolv.conf
		// is the systemd-resolved 127.0.0.53 stub (unreachable from inside the pod); without a kubelet
		// the node must supply pod DNS itself.
		for _, s := range podDNS {
			if s = strings.TrimSpace(s); s != "" {
				backend.DNSServers = append(backend.DNSServers, s)
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

// configureGitHubOverride wires the node's non-github.com git-host lane onto managerCfg from the
// GITHUB_* config (DEV/TEST only; the e2e-vm Gitea lane). With no GitHub config set it is a no-op —
// github: mounts keep the production default (github.com, AS-minted, secure).
//
// When a static token is configured (GITHUB_STATIC_TOKEN or GITHUB_STATIC_TOKEN_FILE) it:
//   - points the repo service at GITHUB_API_BASE_URL (Gitea's /api/v1),
//   - sets the mount Host + AllowInsecureHost so http clone URLs validate,
//   - renders a git credential-helper (echoing the PAT) and installs a StaticGitHubCredentials
//     provider so the node clones without any AS mint.
//
// GITHUB_HOST/GITHUB_ALLOW_INSECURE_HOST may also be set WITHOUT a static token (e.g. a self-hosted
// GitHub Enterprise still using the AS mint); the host override applies either way.
func configureGitHubOverride(managerCfg *spawnlet.ManagerConfig, cfg *Spawnlet) error {
	g := cfg.GitHub
	managerCfg.GitHubHost = strings.TrimSpace(g.Host)
	managerCfg.GitHubAllowInsecureHost = g.AllowInsecureHost

	if base := strings.TrimSpace(g.APIBaseURL); base != "" {
		managerCfg.GitHubRepos = storage.NewDefaultGitHubRepoService(base)
	}

	token, err := githubStaticToken(g.StaticToken, g.StaticTokenFile)
	if err != nil {
		return err
	}
	if token == "" {
		return nil // no static-token lane; host override (if any) still applied above
	}
	helperPath, err := writeGitHubStaticCredentialHelper(cfg.DataRoot, token)
	if err != nil {
		return err
	}
	managerCfg.GitHubStaticCredentials = storage.StaticGitHubCredentials{
		AccessToken:          token,
		CredentialHelperPath: helperPath,
	}
	log.Printf("github: STATIC-TOKEN lane enabled (host=%q, api=%q, insecure=%v) — DEV/TEST ONLY, no AS mint",
		managerCfg.GitHubHost, strings.TrimSpace(g.APIBaseURL), managerCfg.GitHubAllowInsecureHost)
	return nil
}

// githubStaticToken resolves the static PAT from the inline secret or a file path (inline wins).
func githubStaticToken(inline config.Secret, file string) (string, error) {
	if t := strings.TrimSpace(string(inline)); t != "" {
		return t, nil
	}
	if file = strings.TrimSpace(file); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read GITHUB_STATIC_TOKEN_FILE %s: %w", file, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

// writeGitHubStaticCredentialHelper writes a 0600 token file plus an executable git
// credential-helper that cats it and echoes basic-auth for any clone. Both live under
// DataRoot/github-static (node-only, never bind-mounted into the agent), mode 0700 dir. The helper
// reads the token from a file rather than embedding it, so the secret is not baked into the
// executable. Mirrors the github_e2e lane's writeCredHelper — appropriate only for a local git
// server under AllowInsecureHost.
func writeGitHubStaticCredentialHelper(dataRoot, token string) (string, error) {
	dir := filepath.Join(dataRoot, "github-static")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	helperPath := filepath.Join(dir, "git-credential-static")
	// Gitea accepts the PAT as the basic-auth password with any username. The helper ignores stdin
	// and unconditionally echoes the credential — correct for a single-token dev git host.
	script := fmt.Sprintf("#!/bin/sh\ntoken=$(cat %q)\nprintf 'username=git\\npassword=%%s\\n' \"$token\"\n", tokenPath)
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		return "", err
	}
	return helperPath, nil
}

// h2cClient mirrors cmd/spawnctl's: cleartext HTTP/2 for the CP dial.
// nodeCPClient selects the node->CP transport by cfg.Node.AuthMode. insecure (default): the h2c
// client to insecureURL. enforced: load the node's mTLS identity from cfg.Node.IDDir (or enroll
// once via cfg.ASURL + cfg.EnrollToken, pinning cfg.Node.RootCA), and return an mTLS client
// targeting cfg.CP.NodeAddr.
//
// Enrollment uses a fingerprint-bound token (owner-sealed-secrets design §5): the node generates and
// persists its keypair FIRST, then logs the key's SPKI fingerprint. The operator gives that fingerprint
// to the account owner, who mints a token bound to it (IssueBoundEnrollmentToken) over the pinned AS
// connection and hands it back as ENROLL_TOKEN. The node redeems with the SAME key, so a token a
// compromised CP observed cannot be redeemed with a substituted key.
func nodeCPClient(cfg *Spawnlet, insecureURL, nodeID string) (*http.Client, string, error) {
	if cfg.Node.AuthMode != "enforced" {
		return h2cClient(), insecureURL, nil
	}
	dir := cfg.Node.IDDir
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
		asURL, enrollToken := cfg.ASURL, string(cfg.EnrollToken)
		if asURL == "" || enrollToken == "" {
			log.Printf("spawnlet node public-key fingerprint: %s", fp)
			log.Printf("spawnlet: mint a fingerprint-bound enrollment token for the above fingerprint, then set AS_URL + ENROLL_TOKEN")
			return nil, "", fmt.Errorf("enforced mode: no identity in %s and AS_URL/ENROLL_TOKEN unset: %w", dir, err)
		}
		rootCAPath := cfg.Node.RootCA
		if rootCAPath == "" {
			rootCAPath = filepath.Join(dir, "root.pem")
		}
		rootPEM, rerr := os.ReadFile(rootCAPath)
		if rerr != nil {
			return nil, "", fmt.Errorf("enforced mode: pinned NODE_ROOT_CA required for enrollment: %w", rerr)
		}
		res, eerr := authsvc.RunEnrollWithKey(context.Background(), asURL, enrollToken, nodeID, key)
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
	return client, cfg.CP.NodeAddr, nil
}

// nodeSubKeys builds the node's HPKE sub-key holder from its enrolled cert key (sp-2ckv.4 §1), so the
// node can publish a cert-signed sub-key and unseal owner-delivered secrets. Returns nil in insecure
// mode (no identity) or if the on-disk key cannot be loaded/parsed — the node then publishes no sub-key
// and rejects SecretDelivery. The sub-key is signed by the SAME key as the node leaf cert (the RFC 9345
// delegated-credential pattern), so a sealing client verifies it chains to the pinned roots.
func nodeSubKeys(cfg *Spawnlet, nodeID string) *subkey.Node {
	if cfg.Node.AuthMode != "enforced" {
		return nil
	}
	dir := cfg.Node.IDDir
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

func nodeRootPEM(cfg *Spawnlet) []byte {
	if cfg.Node.AuthMode != "enforced" {
		return nil
	}
	dir := cfg.Node.IDDir
	path := cfg.Node.RootCA
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
//
// nodeGitHubMint is driven by the typed cfg.Node config (populated by the node.* YAML, the
// NODE_*/AS_URL env aliases, and --set alike), not by reading the environment directly.
func nodeGitHubMint(cfg *Spawnlet) node.GitHubMintClient {
	asURL := cfg.ASURL
	// D3 dev-github lane: relaxed node->AS over plain HTTP with a header identity (NOT mTLS). The
	// secure mTLS leg is proven by TestGitHubE2E_* and is the enforced/prod path below.
	if devNodeID := strings.TrimSpace(cfg.Node.GitHubMintDevID); devNodeID != "" {
		if asURL == "" {
			log.Printf("github mint: NODE_GITHUB_MINT_DEV_NODE_ID set but AS_URL empty — relaxed mint disabled")
			return nil
		}
		log.Printf("github mint: DEV RELAXED node->AS (plain HTTP, header identity %q) — NOT for production", devNodeID)
		return authv1connect.NewAuthServiceClient(h2cClient(), asURL,
			connect.WithInterceptors(devNodeIDInterceptor{nodeID: devNodeID}))
	}
	if cfg.Node.AuthMode != "enforced" {
		return nil
	}
	if asURL == "" {
		return nil
	}
	dir := cfg.Node.IDDir
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

// buildIntentVerifier builds the A4 IntentVerifier from the config [AC1][AM12].
//
// cfg.Node.AuthMode=insecure (default): AuthModeVerifyLog — the full verification chain runs but
// failures are logged rather than enforced. This satisfies AM12 (dev/prod parity via verify-and-log).
// cfg.Node.AuthMode=enforced: AuthModeEnforced — failures block execution and return NACK codes.
//
// Artifact trust is rooted exclusively in cfg.Node.RootCA (or <id_dir>/root.pem) and the persisted
// signer-revocation store. In insecure mode unavailable trust logs token failures but does not block.
//
// nodeOwner: if non-empty AND cfg.Node.AuthMode=enforced, enables the self-hosted owner check
// (the token's account_id must equal nodeOwner).
func buildIntentVerifier(cfg *Spawnlet, trust *artifactTrust, nodeID, nodeOwner string) (*node.IntentVerifier, error) {
	var authMode node.AuthMode
	switch cfg.Node.AuthMode {
	case "insecure":
		authMode = node.AuthModeVerifyLog
	case "enforced":
		authMode = node.AuthModeEnforced
	default:
		return nil, fmt.Errorf("unknown node auth mode %q", cfg.Node.AuthMode)
	}

	var artifacts *token.Verifier
	if trust != nil {
		artifacts = trust.verifier
	}

	selfHosted := authMode == node.AuthModeEnforced && nodeOwner != ""
	return node.NewIntentVerifier(artifacts, nodeOwner, nodeID, selfHosted, authMode, nil), nil
}

type artifactTrust struct {
	verifier       *token.Verifier
	revocations    *token.SignerRevocationStore
	statementPath  string
	reload         func(context.Context, time.Time) error
	closeStore     func() error
	mu             sync.Mutex
	reloadActive   bool
	reloadFinished <-chan struct{}
}

func loadArtifactVerifier(cfg *Spawnlet, now time.Time) (*artifactTrust, error) {
	rootPath := cfg.Node.RootCA
	if rootPath == "" && cfg.Node.IDDir != "" {
		rootPath = filepath.Join(cfg.Node.IDDir, "root.pem")
	}
	if rootPath == "" || cfg.Node.Environment == "" || cfg.Node.SignerRevocationState == "" {
		return nil, errors.New("root, environment, and signer-revocation state are required")
	}
	rootPEM, err := os.ReadFile(rootPath)
	if err != nil {
		return nil, fmt.Errorf("read environment root: %w", err)
	}
	root, err := pki.ParseCertPEM(rootPEM)
	if err != nil {
		return nil, fmt.Errorf("parse environment root: %w", err)
	}
	store, err := token.OpenSignerRevocationStore(cfg.Node.SignerRevocationState, root, cfg.Node.Environment, now)
	if err != nil {
		return nil, fmt.Errorf("open signer-revocation state: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = store.Close()
		}
	}()
	if cfg.Node.SignerRevocationStatement != "" {
		if err := store.LoadAndApply(cfg.Node.SignerRevocationStatement, now); err != nil {
			return nil, fmt.Errorf("apply signer-revocation statement: %w", err)
		}
	}
	verifier, err := token.NewVerifier(root, cfg.Node.Environment, store)
	if err != nil {
		return nil, fmt.Errorf("create artifact verifier: %w", err)
	}
	closeOnError = false
	trust := &artifactTrust{verifier: verifier, revocations: store, statementPath: cfg.Node.SignerRevocationStatement}
	trust.closeStore = store.Close
	trust.reload = func(ctx context.Context, at time.Time) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		err := store.LoadAndApply(trust.statementPath, at)
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	return trust, nil
}

func (trust *artifactTrust) Close() error {
	if trust == nil {
		return nil
	}
	trust.mu.Lock()
	defer trust.mu.Unlock()
	if trust.reloadActive {
		return errors.New("signer-revocation reload is still active")
	}
	return trust.closeStore()
}

func (trust *artifactTrust) CloseContext(ctx context.Context) error {
	if trust == nil {
		return nil
	}
	for {
		trust.mu.Lock()
		if !trust.reloadActive {
			err := trust.closeStore()
			trust.mu.Unlock()
			return err
		}
		finished := trust.reloadFinished
		trust.mu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for signer-revocation reload: %w", ctx.Err())
		case <-finished:
		}
	}
}

func (trust *artifactTrust) watch(ctx context.Context, interval time.Duration, now func() time.Time, report func(error)) <-chan struct{} {
	done := make(chan struct{})
	if trust == nil || trust.statementPath == "" {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var reloadDone <-chan error
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-reloadDone:
				reloadDone = nil
				if err != nil && !errors.Is(err, context.Canceled) && report != nil {
					report(err)
				}
			case <-ticker.C:
				if reloadDone != nil {
					continue
				}
				result := make(chan error, 1)
				finished := make(chan struct{})
				trust.mu.Lock()
				trust.reloadActive = true
				trust.reloadFinished = finished
				trust.mu.Unlock()
				reloadDone = result
				go func(at time.Time) {
					err := trust.reload(ctx, at)
					trust.mu.Lock()
					trust.reloadActive = false
					trust.mu.Unlock()
					close(finished)
					result <- err
				}(now())
			}
		}
	}()
	return done
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

// (the env/getenvBool helpers were removed — configureJournal and nodeGitHubMint now read the
// typed config, not the process environment.)
