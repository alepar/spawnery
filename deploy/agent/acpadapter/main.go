package main

import (
	"log"
	"net"
	"os"
	"os/exec"
)

// acpadapter starts the agent given by its args (e.g. `goose acp`), listens on
// the abstract socket @spawnlet-acp (or $ACP_SOCKET), and bridges the current
// client connection to the agent's stdio. Exits when the agent exits.
func main() {
	log.SetPrefix("acpadapter: ")
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: acpadapter <agent-cmd> [args...]")
	}
	sock := os.Getenv("ACP_SOCKET")
	if sock == "" {
		sock = "@spawnlet-acp" // leading @ = Linux abstract namespace
	}

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

	ln, err := net.Listen("unix", sock)
	if err != nil {
		log.Fatalf("listen %s: %v", sock, err)
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
