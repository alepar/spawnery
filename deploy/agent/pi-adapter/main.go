package main

import (
	"log"
	"net"
	"os"
	"strings"

	"spawnery/internal/piadapter"
)

// pi-adapter presents a canonical-ACP agent to the spawnery node, backed by a
// `pi --mode rpc` child process the adapter spawns and owns. It mirrors
// deploy/agent/acpadapter (opencode) but spawns pi over stdio instead of
// talking to an HTTP server.
//
// Environment:
//   - ACP_LISTEN: "tcp://host:port" (preferred) or "unix:<path>".
//     Defaults to a unix abstract socket @spawnlet-acp for back-compat.
//   - PI_DIR: the spawn working dir (default /app), used as pi's cwd.
//   - SPAWN_MODEL: the bare model id wired into pi's "spawnery" provider (optional).
//   - PI_BIN: the pi binary path/name (default "pi"); overridable for tests/e2e.
//
// It serves one node connection at a time; on disconnect it loops to accept the next.
func main() {
	log.SetPrefix("pi-adapter: ")
	log.SetFlags(0)

	network, addr := listenSpec()
	dir := envOr("PI_DIR", "/app")
	model := os.Getenv("SPAWN_MODEL")
	piBin := envOr("PI_BIN", "pi")

	ln, err := net.Listen(network, addr)
	if err != nil {
		log.Fatalf("listen %s %s: %v", network, addr, err)
	}
	log.Printf("listening on %s %s, pi binary %q, dir %q", network, addr, piBin, dir)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatalf("accept: %v", err)
		}
		a := piadapter.New(model, dir, piadapter.WithBinary(piBin))
		if serr := a.Serve(conn, conn); serr != nil {
			log.Printf("connection ended: %v", serr)
		}
		_ = conn.Close()
	}
}

// listenSpec resolves the (network, address) the adapter listens on. $ACP_LISTEN
// takes precedence ("tcp://host:port" | "unix:<path>"); otherwise a Unix socket
// at $ACP_SOCKET, defaulting to the abstract @spawnlet-acp.
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
