package spawnlet

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// sessionTitle builds the opencode session title shown in the TUI/web: "<spawn name> (<app title>)",
// omitting whichever part is empty. Returns "" only when both are empty (the adapter then falls back
// to a default). Bracket form because opencode session titles are single-line.
func sessionTitle(name, appTitle string) string {
	name, appTitle = strings.TrimSpace(name), strings.TrimSpace(appTitle)
	switch {
	case name != "" && appTitle != "":
		return name + " (" + appTitle + ")"
	case name != "":
		return name
	default:
		return appTitle
	}
}

// Terminal attach: spawnctl tmux <spawn> -> CP -> node. The node starts a mosh-server whose child
// execs into the spawn's agent container and runs a persistent tmux session hosting the opencode
// TUI, attached to the in-pod opencode server. The TUI and the web UI then share one
// server-authoritative session (see sp-wsu). mosh's UDP data plane goes straight to the node.

const (
	defaultAttachURL   = "http://127.0.0.1:4096"
	defaultTmuxSession = "opencode"
)

// TerminalSession is the mosh connect info handed back (via CP) to spawnctl.
type TerminalSession struct {
	Host string // node address spawnctl's mosh-client dials
	Port int    // mosh UDP port
	Key  string // mosh AES key (MOSH_KEY)
}

// TerminalConfig configures the terminal launcher per lane/node.
type TerminalConfig struct {
	ExecPrefix  []string // runtime exec invocation, e.g. ["docker","exec","-it"] or ["crictl","exec","-it"]
	AttachURL   string   // opencode server URL inside the container (default 127.0.0.1:4096)
	Session     string   // tmux session name (default "opencode")
	OcSessionID string   // the spawn's opencode session id (so the TUI lands on the shared session)
	AdvertiseIP string   // node IP mosh advertises to the client; "" => mosh auto-detects
}

// attachCommand is the in-container command: the baked `spawn-tui` launcher (deploy/agent/spawn-tui.sh).
// It sets TERM (else the full-screen TUI half-renders over the mosh PTY) and pins
// `opencode attach -s <id>` to the spawn's shared opencode session (the adapter writes that id to a
// file; `opencode attach -c` does NOT reliably select it). `tmux new-session -A` reattaches if the
// session already exists.
func attachCommand() []string {
	return []string{"spawn-tui"}
}

// execArgv prefixes the in-container command with the runtime's exec invocation + container id.
func execArgv(execPrefix []string, containerID string, inner []string) []string {
	argv := make([]string, 0, len(execPrefix)+1+len(inner))
	argv = append(argv, execPrefix...)
	argv = append(argv, containerID)
	return append(argv, inner...)
}

var moshConnectRE = regexp.MustCompile(`MOSH CONNECT (\d+) (\S+)`)

// parseMoshConnect extracts the port + key from mosh-server's "MOSH CONNECT <port> <key>" line.
func parseMoshConnect(out string) (port int, key string, err error) {
	m := moshConnectRE.FindStringSubmatch(out)
	if m == nil {
		return 0, "", fmt.Errorf("no MOSH CONNECT line in mosh-server output: %q", out)
	}
	port, _ = strconv.Atoi(m[1])
	return port, m[2], nil
}

// moshServerArgs builds the mosh-server argv for the given child command.
func moshServerArgs(advertiseIP string, child []string) []string {
	args := []string{"new"}
	if advertiseIP != "" {
		args = append(args, "-i", advertiseIP)
	}
	args = append(args, "--")
	return append(args, child...)
}

// ExecPrefixFor returns the runtime exec invocation for a lane. runsc/CRI uses crictl; the Docker
// (runc) lane uses the docker CLI. -it gives the in-container process a TTY (tmux/shell need one;
// mosh supplies the outer PTY). TERM is forwarded so full-screen programs (the opencode TUI, a
// shell's editor) render correctly.
func ExecPrefixFor(runtimeKind string) []string {
	if runtimeKind == "runsc" {
		return []string{"crictl", "exec", "-it"}
	}
	return []string{"docker", "exec", "-it", "-e", "TERM=xterm-256color"}
}

// StartTerminal (Manager method) looks up the spawn and launches a terminal session for it. cmd is
// the in-container command (nil/empty => the opencode TUI launcher; a command => raw exec, e.g.
// /bin/bash). Raw exec is an un-audited mutation path (bypasses the sidecar) — owner-only.
func (m *Manager) StartTerminal(ctx context.Context, spawnID string, cmd []string) (TerminalSession, error) {
	sp, ok := m.store.Get(spawnID)
	if !ok {
		return TerminalSession{}, fmt.Errorf("spawn not found: %s", spawnID)
	}
	if sp.AgentID == "" {
		return TerminalSession{}, fmt.Errorf("spawn %s has no agent container", spawnID)
	}
	return StartTerminal(ctx, sp.AgentID, cmd, TerminalConfig{
		ExecPrefix:  ExecPrefixFor(m.cfg.ContainerRuntime),
		AdvertiseIP: m.cfg.AdvertiseIP,
	})
}

// StartTerminal launches a mosh-server bound to an in-container command execed into the spawn's
// container, and returns the mosh connect info for spawnctl. cmd nil/empty => the opencode TUI
// launcher (spawn-tui); otherwise the given command (raw exec).
func StartTerminal(ctx context.Context, containerID string, cmd []string, cfg TerminalConfig) (TerminalSession, error) {
	inner := cmd
	if len(inner) == 0 {
		inner = attachCommand()
	}
	child := execArgv(cfg.ExecPrefix, containerID, inner)
	out, err := exec.CommandContext(ctx, "mosh-server", moshServerArgs(cfg.AdvertiseIP, child)...).Output()
	if err != nil {
		return TerminalSession{}, fmt.Errorf("mosh-server: %w", err)
	}
	port, key, perr := parseMoshConnect(string(out))
	if perr != nil {
		return TerminalSession{}, perr
	}
	return TerminalSession{Host: cfg.AdvertiseIP, Port: port, Key: key}, nil
}
