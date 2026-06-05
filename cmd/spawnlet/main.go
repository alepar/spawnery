package main

import (
	"context"
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

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/authsvc"
	"spawnery/internal/node"
	"spawnery/internal/node/nodeid"
	"spawnery/internal/runtime"
	"spawnery/internal/runtime/cri"
	"spawnery/internal/spawnlet"
	"spawnery/internal/spawnlet/firewall"
)

func main() {
	cfg := spawnlet.ManagerConfig{
		AgentImage:    env("AGENT_IMAGE", "spawnery/stubagent:dev"),
		SidecarImage:  env("SIDECAR_IMAGE", "spawnery/sidecar:dev"),
		OpenRouterKey: os.Getenv("OPENROUTER_API_KEY"),
		DataRoot:      env("DATA_ROOT", "/var/lib/spawnlet/spawns"),

		NodeID:           env("NODE_ID", "node-1"),
		NodeClass:        env("NODE_CLASS", "cloud"),
		EgressEnforce:    getenvBool("EGRESS_ENFORCE", true),
		EgressAllowCIDRs: splitCSV(os.Getenv("EGRESS_ALLOW_CIDRS")),

		MemLimitMB:       getenvInt64("MEM_LIMIT_MB", 1024),
		CPULimit:         getenvFloat("CPU_LIMIT", 1.0),
		PidsLimit:        getenvInt64("PIDS_LIMIT", 256),
		ContainerRuntime: os.Getenv("CONTAINER_RUNTIME"),
		HardenRootfs:     getenvBool("HARDEN_ROOTFS", false),
		AdvertiseIP:      env("NODE_ADVERTISE_IP", "127.0.0.1"),
	}
	mgr, err := buildManager(cfg)
	if err != nil {
		log.Fatalf("manager init: %v", err)
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
			NodeID:     env("NODE_ID", "node-1"),
			CPURL:      cpURL,
			MaxSpawns:  4,
			AgentImage: env("AGENT_IMAGE", "spawnery/stubagent:dev"),
			NodeClass:  env("NODE_CLASS", "cloud"),
			NodeOwner:  env("NODE_OWNER", ""),
		}
		// Terminal control plane (around CP for now): a small inbound listener so `spawnctl tmux`
		// can ask this node to start a mosh-backed terminal session for a spawn. The mosh UDP data
		// plane goes straight to this node. (CP-routed terminal control is sp-wsu.2.)
		if taddr := env("NODE_TERMINAL_ADDR", "127.0.0.1:9092"); taddr != "" {
			tsrv := spawnlet.NewServer(mgr)
			tmux := http.NewServeMux()
			tmux.HandleFunc("/terminal", tsrv.HandleTerminal)
			go func() {
				log.Printf("spawnlet terminal endpoint on %s (spawnctl tmux -addr http://%s)", taddr, taddr)
				if err := http.ListenAndServe(taddr, tmux); err != nil {
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
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(srv))
	mux.HandleFunc("/ws/session", srv.HandleWS)
	mux.HandleFunc("/terminal", srv.HandleTerminal)
	addr := env("SPAWNLET_ADDR", "127.0.0.1:9090")
	log.Printf("spawnlet listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h2c.NewHandler(mux, &http2.Server{})))
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
func buildManager(cfg spawnlet.ManagerConfig) (*spawnlet.Manager, error) {
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
	return spawnlet.NewManager(rt, cfg), nil
}

// h2cClient mirrors cmd/spawnctl's: cleartext HTTP/2 for the CP dial.
// nodeCPClient selects the node->CP transport by NODE_AUTH_MODE. insecure (default): the h2c client to
// CP_ADDR. enforced: load the node's mTLS identity from NODE_ID_DIR (or enroll once via AS_URL +
// ENROLL_TOKEN, pinning NODE_ROOT_CA), and return an mTLS client targeting CP_NODE_ADDR.
func nodeCPClient(insecureURL, nodeID string) (*http.Client, string, error) {
	if env("NODE_AUTH_MODE", "insecure") != "enforced" {
		return h2cClient(), insecureURL, nil
	}
	dir := env("NODE_ID_DIR", "/var/lib/spawnlet/identity")
	id, err := nodeid.Load(dir)
	if err != nil {
		asURL, token := os.Getenv("AS_URL"), os.Getenv("ENROLL_TOKEN")
		if asURL == "" || token == "" {
			return nil, "", fmt.Errorf("enforced mode: no identity in %s and AS_URL/ENROLL_TOKEN unset: %w", dir, err)
		}
		rootPEM, rerr := os.ReadFile(env("NODE_ROOT_CA", filepath.Join(dir, "root.pem")))
		if rerr != nil {
			return nil, "", fmt.Errorf("enforced mode: pinned NODE_ROOT_CA required for enrollment: %w", rerr)
		}
		res, eerr := authsvc.RunEnroll(context.Background(), asURL, token, nodeID)
		if eerr != nil {
			return nil, "", fmt.Errorf("enroll: %w", eerr)
		}
		id = nodeid.Identity{CertPEM: res.CertPEM, ChainPEM: res.ChainPEM, KeyPEM: res.KeyPEM, RootPEM: rootPEM}
		if serr := nodeid.Save(dir, id); serr != nil {
			return nil, "", fmt.Errorf("persist identity: %w", serr)
		}
		log.Printf("spawnlet enrolled with AS %s; identity stored in %s", asURL, dir)
	}
	client, err := id.MTLSClient()
	if err != nil {
		return nil, "", err
	}
	return client, env("CP_NODE_ADDR", "https://127.0.0.1:8081"), nil
}

func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
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
