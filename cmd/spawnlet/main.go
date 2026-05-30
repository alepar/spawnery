package main

import (
	"log"
	"net/http"
	"os"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"spawnery/gen/spawn/v1/spawnv1connect"
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
	})
	srv := spawnlet.NewServer(mgr)
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(srv))
	mux.HandleFunc("/ws/session", srv.HandleWS)

	addr := env("SPAWNLET_ADDR", "127.0.0.1:9090")
	log.Printf("spawnlet listening on %s", addr)
	// h2c so the Go client can use HTTP/2 bidi without TLS for the slice.
	log.Fatal(http.ListenAndServe(addr, h2c.NewHandler(mux, &http2.Server{})))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
