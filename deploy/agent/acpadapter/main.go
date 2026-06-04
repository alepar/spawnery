package main

import (
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
)

// acpadapter starts the agent given by its args (e.g. `goose acp`), listens for
// the node, and bridges the current client connection to the agent's stdio.
// Exits when the agent exits.
//
// Transport (lowest precedence first):
//   - default: abstract Unix socket @spawnlet-acp (the runc/shared-netns lane).
//   - $ACP_SOCKET: an explicit Unix socket path/name (overrides the default).
//   - $ACP_LISTEN: "tcp://host:port" or "unix:<path>" — wins over the above. The
//     runsc/CRI lane sets tcp:// because gVisor isolates the in-sandbox abstract
//     socket namespace from the host, so the node cannot reach a UDS via setns;
//     it dials the pod IP instead.
func main() {
	log.SetPrefix("acpadapter: ")
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: acpadapter <agent-cmd> [args...]")
	}
	network, addr := listenSpec()

	agent := exec.Command(os.Args[1], os.Args[2:]...)
	agent.Stderr = os.Stderr
	toAgent, err := agent.StdinPipe()
	if err != nil {
		log.Fatalf("agent stdin: %v", err)
	}
	fromAgent, err := agent.StdoutPipe()
	if err != nil {
		log.Fatalf("agent stdout: %v", err)
	}
	if err := agent.Start(); err != nil {
		log.Fatalf("start agent: %v", err)
	}

	ln, err := net.Listen(network, addr)
	if err != nil {
		log.Fatalf("listen %s %s: %v", network, addr, err)
	}

	// The spawn is over when the agent exits.
	go func() {
		werr := agent.Wait()
		log.Printf("agent exited: %v", werr)
		_ = ln.Close()
		os.Exit(0)
	}()

	if err := serve(ln, toAgent, fromAgent); err != nil {
		log.Printf("serve ended: %v", err)
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
