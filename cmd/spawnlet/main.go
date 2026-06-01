package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/node"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

func main() {
	rt, err := runtime.NewDocker()
	if err != nil {
		log.Fatalf("docker: %v", err)
	}
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage:    env("AGENT_IMAGE", "spawnery/stubagent:dev"),
		SidecarImage:  env("SIDECAR_IMAGE", "spawnery/sidecar:dev"),
		OpenRouterKey: os.Getenv("OPENROUTER_API_KEY"),
		DataRoot:      env("DATA_ROOT", "/var/lib/spawnlet/spawns"),

		NodeClass:        env("NODE_CLASS", "cloud"),
		EgressEnforce:    getenvBool("EGRESS_ENFORCE", true),
		EgressAllowCIDRs: splitCSV(os.Getenv("EGRESS_ALLOW_CIDRS")),

		MemLimitMB:       getenvInt64("MEM_LIMIT_MB", 1024),
		CPULimit:         getenvFloat("CPU_LIMIT", 1.0),
		PidsLimit:        getenvInt64("PIDS_LIMIT", 256),
		ContainerRuntime: os.Getenv("CONTAINER_RUNTIME"),
	})
	ctx := context.Background()
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
		log.Printf("spawnlet attaching to CP at %s as %s", cpURL, cfg.NodeID)
		log.Fatal(node.Run(ctx, mgr, h2cClient(), cfg))
	}

	// Standalone mode (unchanged): inbound spawn.v1 server + /ws.
	srv := spawnlet.NewServer(mgr)
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(srv))
	mux.HandleFunc("/ws/session", srv.HandleWS)
	addr := env("SPAWNLET_ADDR", "127.0.0.1:9090")
	log.Printf("spawnlet listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h2c.NewHandler(mux, &http2.Server{})))
}

// h2cClient mirrors cmd/spawnctl's: cleartext HTTP/2 for the CP dial.
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
