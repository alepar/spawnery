package main

import (
	"log"
	"net"
	"os"
	"strings"

	"spawnery/internal/ocadapter"
	"spawnery/internal/opencode"
)

// acpadapter presents a canonical-ACP agent to the spawnery node, backed by an
// in-pod `opencode serve` instance. opencode is launched separately by the
// container entrypoint; this process only listens for the node and translates.
//
// Environment:
//   - ACP_LISTEN: "tcp://host:port" (preferred, both lanes) or "unix:<path>".
//     Defaults to a unix abstract socket @spawnlet-acp for back-compat.
//   - OPENCODE_BASE_URL: the in-pod opencode server (default http://127.0.0.1:4096).
//   - OPENCODE_DIR: the spawn working dir used to scope session discovery (default /app).
//   - SPAWN_MODEL: "providerID/modelID" forwarded on prompts (optional).
//
// It serves one node connection at a time (the node holds a single long-lived
// connection per spawn); on disconnect it loops to accept the next.
func main() {
	log.SetPrefix("acpadapter: ")
	log.SetFlags(0)

	network, addr := listenSpec()
	baseURL := envOr("OPENCODE_BASE_URL", "http://127.0.0.1:4096")
	dir := envOr("OPENCODE_DIR", "/app")
	model := os.Getenv("SPAWN_MODEL")

	ln, err := net.Listen(network, addr)
	if err != nil {
		log.Fatalf("listen %s %s: %v", network, addr, err)
	}
	log.Printf("listening on %s %s, opencode at %s", network, addr, baseURL)

	oc := opencode.New(baseURL)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatalf("accept: %v", err)
		}
		a := ocadapter.New(oc, dir, model)
		if serr := a.Serve(conn, conn); serr != nil {
			log.Printf("connection ended: %v", serr)
		}
		_ = conn.Close()
	}
}

// listenSpec resolves the (network, address) the adapter listens on from the
// environment. $ACP_LISTEN takes precedence ("tcp://host:port" | "unix:<path>");
// otherwise it's a Unix socket at $ACP_SOCKET, defaulting to the abstract
// @spawnlet-acp.
func listenSpec() (network, addr string) {
	if l := os.Getenv("ACP_LISTEN"); l != "" {
		if rest, ok := strings.CutPrefix(l, "tcp://"); ok {
			return "tcp", rest
		}
		if rest, ok := strings.CutPrefix(l, "unix:"); ok {
			return "unix", rest
		}
		log.Fatalf("ACP_LISTEN must be tcp://host:port or unix:<path>, got %q", l)
	}
	sock := os.Getenv("ACP_SOCKET")
	if sock == "" {
		sock = "@spawnlet-acp" // leading @ = Linux abstract namespace
	}
	return "unix", sock
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
