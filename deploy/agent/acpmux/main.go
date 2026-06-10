// acpmux is the in-container single-session ACP multiplexer. It spawns the given
// agent argv (e.g. `goose acp` — canonical ACP over stdio), is its SINGLE ACP
// client (one shared session, eager initialize + session/new at startup), and
// presents canonical ACP as an agent on ACP_LISTEN (tcp:// or unix:), accepting
// MULTIPLE concurrent downstream clients and multiplexing them onto that one
// upstream session (replay to late joiners, notification fanout, serialized
// prompts, broadcast permissions — see internal/acpmux).
//
// Unlike acpexec (one child per connection, raw byte relay), acpmux spawns the
// agent ONCE and shares it across all connections. It replaces acpexec on :7000
// for the goose-acp runnable so the web (via the node pump) and an in-container
// nori (via acpdial, sp-9xr.12.2) share one goose conversation.
//
// Usage: acpmux <agent> [args...]   e.g.  acpmux goose acp
//
// Environment:
//   - ACP_LISTEN: "tcp://host:port" (preferred) or "unix:<path>". Falls back to
//     a Unix socket at $ACP_SOCKET (default abstract @spawnlet-acp).
//
// The agent inherits this process's environment (the dispatcher exports the
// agent's provider config — GOOSE_PROVIDER, OPENAI_BASE_URL, etc. — before exec).
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"spawnery/internal/acpmux"
)

func main() {
	log.SetPrefix("acpmux: ")
	log.SetFlags(0)

	argv := os.Args[1:]
	if len(argv) == 0 {
		log.Fatalf("usage: acpmux <agent> [args...]")
	}

	network, addr := listenSpec()

	// Spawn the upstream agent with stdio pipes. We use pipes (not the conn) so
	// cmd.Wait() returns the instant the agent exits regardless of any blocked
	// copy — and we ALWAYS Wait() to reap it (the .6 zombie lesson).
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("stdin pipe for %v: %v", argv, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("stdout pipe for %v: %v", argv, err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("start agent %v: %v", argv, err)
	}

	mux := acpmux.New(stdin, stdout)

	// Eager handshake: initialize + session/new -> the shared session S.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	if serr := mux.Start(ctx, 120*time.Second); serr != nil {
		cancel()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Fatalf("upstream agent %v handshake: %v", argv, serr)
	}
	cancel()

	ln, err := net.Listen(network, addr)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Fatalf("listen %s %s: %v", network, addr, err)
	}
	log.Printf("listening on %s %s, agent %v, session %s ready", network, addr, argv, mux.SessionID())

	// Reap the agent child in the background; on agent death, tear down the mux
	// and exit so the supervisor restarts the pod (no zombies, no wedged accept).
	go func() {
		werr := cmd.Wait()
		log.Printf("agent %v exited: %v", argv, werr)
		mux.Stop()
		_ = ln.Close()
		os.Exit(1)
	}()

	// Serve downstream clients until a fatal accept error (e.g. ln closed on
	// agent death above).
	if serr := mux.Serve(ln); serr != nil {
		log.Printf("serve: %v", serr)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// listenSpec resolves the (network, address) acpmux listens on from the
// environment. $ACP_LISTEN takes precedence ("tcp://host:port" | "unix:<path>");
// otherwise a Unix socket at $ACP_SOCKET, defaulting to the abstract
// @spawnlet-acp. Mirrors acpexec's parser (intentional small duplication to keep
// the agent tools independent).
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
