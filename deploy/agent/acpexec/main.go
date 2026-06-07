package main

import (
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
)

// acpexec is a generic stdio-ACP <-> TCP bridge. Some agents (e.g. `goose acp`)
// speak canonical ACP (JSON-RPC: initialize, session/new, session/prompt) over
// stdio, but the spawnery node attaches ACP over a socket (ACP_LISTEN, dialed by
// the node's Pump). acpexec listens on ACP_LISTEN, accepts the node's single
// long-lived connection, runs the given agent argv with its stdin/stdout wired
// directly to that connection, and relays bytes raw. On node disconnect it kills
// the child and loops to accept the next connection (a fresh agent process per
// connection — non-lossless, consistent with the rest of the system).
//
// Usage: acpexec <agent> [args...]   e.g.  acpexec goose acp
//
// Environment:
//   - ACP_LISTEN: "tcp://host:port" (preferred) or "unix:<path>". Falls back to
//     a Unix socket at $ACP_SOCKET (default abstract @spawnlet-acp).
//
// The agent inherits this process's environment (the dispatcher exports the
// agent's provider config — GOOSE_PROVIDER, OPENAI_BASE_URL, etc. — before exec).
func main() {
	log.SetPrefix("acpexec: ")
	log.SetFlags(0)

	argv := os.Args[1:]
	if len(argv) == 0 {
		log.Fatalf("usage: acpexec <agent> [args...]")
	}

	network, addr := listenSpec()
	ln, err := net.Listen(network, addr)
	if err != nil {
		log.Fatalf("listen %s %s: %v", network, addr, err)
	}
	log.Printf("listening on %s %s, agent %v", network, addr, argv)

	if serr := serve(ln, argv); serr != nil {
		log.Fatalf("serve: %v", serr)
	}
}

// serve accepts node connections one at a time and, for each, runs the agent argv
// with stdin/stdout wired to the connection. It returns only on a fatal accept
// error (the production loop runs forever). Factored out so tests can drive it
// with a fake agent (e.g. `cat`) on a test listener.
func serve(ln net.Listener, argv []string) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		runChild(conn, argv)
		_ = conn.Close()
	}
}

// runChild starts the agent with its stdin/stdout wired directly to conn and
// blocks until the child exits. Because cmd.Stdin == conn, the child sees stdin
// EOF when the node closes the connection and (for well-behaved agents) exits;
// the deferred Kill is the safety net for a child that ignores stdin EOF. It
// always Waits the child so there are no zombies (the .6 zombie lesson).
func runChild(conn net.Conn, argv []string) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = conn
	cmd.Stdout = conn
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		log.Printf("start agent %v: %v", argv, err)
		return
	}
	// Safety net: ensure the child is gone before we return so a fresh one starts
	// on the next accept. Kill is a no-op (error ignored) if it already exited.
	defer func() { _ = cmd.Process.Kill() }()

	// Wait for the child to exit. When the node closes conn, the child's stdin
	// hits EOF and it exits; Wait returns and we tear down + loop.
	if err := cmd.Wait(); err != nil {
		log.Printf("agent %v exited: %v", argv, err)
	}
}

// listenSpec resolves the (network, address) the bridge listens on from the
// environment. $ACP_LISTEN takes precedence ("tcp://host:port" | "unix:<path>");
// otherwise it's a Unix socket at $ACP_SOCKET, defaulting to the abstract
// @spawnlet-acp. Mirrors acpadapter's parser (intentional small duplication to
// keep the agent tools independent).
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
