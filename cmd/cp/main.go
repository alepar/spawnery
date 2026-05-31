package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/cp"
	"spawnery/internal/cp/apps"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/telemetry"
)

func main() {
	reg := registry.New()
	rt := router.New()
	sched := scheduler.New(reg, rt, 60*time.Second)
	appMap := apps.New(map[string]string{
		"secret-app": "examples/secret-app",
	})
	authn := auth.New(parseTokens(env("CP_DEV_TOKENS", "dev-token=dev")))

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

	srv := cp.NewServer(reg, rt, sched, appMap, tel)

	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(srv)) // node side: no auth (internal nodes)
	mux.Handle(cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(authn.Interceptor())))
	mux.HandleFunc("/ws/session", srv.HandleWS(authn))

	addr := env("CP_LISTEN", "127.0.0.1:8080")
	log.Printf("cp listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h2c.NewHandler(mux, &http2.Server{})))
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
