package main

import (
	"io"
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
// to that connection, and relays bytes raw. On node disconnect it kills the
// child and loops to accept the next connection (a fresh agent process per
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
		// runChild owns conn and closes it before returning; the extra Close here
		// is a harmless idempotent safety net (error ignored).
		runChild(conn, argv)
		_ = conn.Close()
	}
}

// runChild starts the agent and relays bytes between conn and the child's
// stdin/stdout with explicit copy goroutines, blocking until the child exits.
//
// We deliberately use cmd.StdinPipe/StdoutPipe + io.Copy rather than wiring
// cmd.Stdin/cmd.Stdout = conn directly. Because conn is a net.Conn (not an
// *os.File), os/exec would spawn internal copy goroutines AND make cmd.Wait()
// block until they finish. If the child exits before the node closes conn (goose
// crash / OOM / auth error / panic) while the node's Pump holds its long-lived
// conn open and idle, that internal node->child copy stays blocked on
// conn.Read() forever, so cmd.Wait() never returns, runChild never returns, the
// deferred Kill never fires, and the accept loop is wedged — the bridge can
// never accept the node's reconnect (the .16 review deadlock).
//
// With pipes, cmd.Wait() does NOT wait on our copy goroutines, so it returns the
// instant the child exits. We then Close conn to unblock the node->child copy
// reading from it. cmd.Wait always runs so there are no zombies (the .6 zombie
// lesson), and the conn.Close + EOF on the pipes ensure no goroutine leaks
// across connections.
func runChild(conn net.Conn, argv []string) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("stdin pipe for %v: %v", argv, err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("stdout pipe for %v: %v", argv, err)
		_ = stdin.Close()
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("start agent %v: %v", argv, err)
		return
	}
	// Safety net: ensure the child is gone before we return so a fresh one starts
	// on the next accept. Kill is a no-op (error ignored) if it already exited.
	defer func() { _ = cmd.Process.Kill() }()

	// node -> child: closing stdin propagates EOF so a well-behaved agent exits
	// when the node disconnects.
	go func() {
		_, _ = io.Copy(stdin, conn)
		_ = stdin.Close()
	}()
	// child -> node.
	go func() {
		_, _ = io.Copy(conn, stdout)
	}()

	// Wait returns the instant the child exits (pipes mean it does not block on
	// our copy goroutines).
	if err := cmd.Wait(); err != nil {
		log.Printf("agent %v exited: %v", argv, err)
	}
	// Unblock the node->child copy goroutine still reading from conn (the child
	// may have exited before the node closed its end). This also unblocks the
	// child->node copy via stdout EOF from the closed pipe.
	_ = conn.Close()
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
