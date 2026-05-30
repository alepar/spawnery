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
	if key == "" {
		log.Fatal("OPENROUTER_API_KEY required")
	}
	log.Printf("sidecar listening on %s -> %s", addr, upstream)
	log.Fatal(http.ListenAndServe(addr, sidecar.NewHandler(upstream, key)))
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
