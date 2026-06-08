package main

import (
	"log"
	"net/http"
	"os"

	"spawnery/internal/sidecar"
)

func main() {
	upstream := getenv("SIDECAR_UPSTREAM", "https://openrouter.ai/api")
	key := os.Getenv("OPENROUTER_API_KEY")
	addr := getenv("SIDECAR_ADDR", "127.0.0.1:8080")
	controlToken := os.Getenv("SIDECAR_CONTROL_TOKEN")
	controlAddr := os.Getenv("SIDECAR_CONTROL_ADDR")
	if key == "" {
		log.Fatal("OPENROUTER_API_KEY required")
	}
	log.Printf("sidecar listening on %s -> %s", addr, upstream)

	// Live model override shared by both proxy paths; empty => passthrough.
	ov := &sidecar.Override{}

	// /v1/messages is the Anthropic Messages API converter (Claude Code); everything else is
	// the transparent OpenAI passthrough (opencode/goose).
	mux := http.NewServeMux()
	mux.Handle("/v1/messages", sidecar.NewMessagesHandler(upstream, key, ov))
	mux.Handle("/", sidecar.NewHandler(upstream, key, ov))

	// Control server: a second listener on the pod IP (not loopback) so the node can set the
	// override. Started only when both a token and an address are configured.
	if controlToken != "" && controlAddr != "" {
		go func() {
			log.Printf("sidecar control listening on %s", controlAddr)
			log.Fatal(http.ListenAndServe(controlAddr, sidecar.NewControlHandler(ov, controlToken)))
		}()
	} else {
		log.Printf("sidecar control endpoint disabled (set SIDECAR_CONTROL_TOKEN and SIDECAR_CONTROL_ADDR to enable)")
	}

	log.Fatal(http.ListenAndServe(addr, mux))
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
