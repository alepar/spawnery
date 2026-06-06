// Package agentcaps is the hardcoded, shared (CP + node) registry mapping a known
// agent binary to its runnables. A runnable fixes a run mode (acp|tmux|served), a
// relay path, the launch/resume argv, and a display label — so spawnery can derive
// web rendering and spawnctl behavior per spawn. See
// docs/superpowers/specs/2026-06-06-tmux-terminal-mode-design.md.
package agentcaps

// Mode is the run mode a runnable produces. It is the (protocol, multiclient-broker)
// bundle that dictates web rendering and spawnctl behavior.
type Mode string

const (
	ModeACP    Mode = "acp"    // ACP over stdio; node Pump brokers; web = ChatView.
	ModeTmux   Mode = "tmux"   // TUI in tmux; tmux brokers; web = xterm.js terminal.
	ModeServed Mode = "served" // agent self-serves (opencode); web = ChatView via ocadapter.
)

// Relay names the in-process relay implementation that carries this runnable's bytes.
type Relay string

const (
	RelayPump      Relay = "pump"      // internal/node Pump (acp)
	RelayOcadapter Relay = "ocadapter" // internal/ocadapter (served)
	RelayRawPTY    Relay = "raw-pty"   // node ws->PTY bridge (tmux); built in sp-9xr.6
)

// Runnable is one way to launch an agent binary, fixing exactly one mode.
type Runnable struct {
	ID     string   // unique within its binary, e.g. "goose-tui"
	Mode   Mode     // acp | tmux | served
	Launch []string // argv to start the agent; executed by the node only for tmux mode in v1
	Resume []string // argv to resume a conversation; nil = none wired yet (see sp-9xr.10)
	Relay  Relay    // which relay carries this runnable's bytes
	Label  string   // human-readable, shown in the runnable dropdown
}

// registry is the single source of truth: binary name -> its runnables.
// Exact acp/served Launch argvs and all Resume argvs are validated when each runnable
// is wired end-to-end (sp-9xr.5 / sp-9xr.8 / sp-9xr.10); v1 only execs tmux-mode Launch.
var registry = map[string][]Runnable{
	"goose": {
		{ID: "goose-acp", Mode: ModeACP, Launch: []string{"goose", "acp"}, Relay: RelayPump, Label: "Goose · rich web"},
		{ID: "goose-tui", Mode: ModeTmux, Launch: []string{"goose"}, Relay: RelayRawPTY, Label: "Goose · terminal"},
	},
	"opencode": {
		{ID: "opencode-served", Mode: ModeServed, Launch: []string{"opencode", "serve", "--port", "4096", "--hostname", "127.0.0.1"}, Relay: RelayOcadapter, Label: "opencode"},
	},
	"claude-code": {
		{ID: "claude-tui", Mode: ModeTmux, Launch: []string{"claude"}, Relay: RelayRawPTY, Label: "Claude Code · terminal"},
	},
}

// Runnables returns the runnables for a known binary. ok is false for an unknown binary.
func Runnables(binary string) (rs []Runnable, ok bool) {
	rs, ok = registry[binary]
	return rs, ok
}

// Lookup resolves a specific (binary, runnableID) pair.
func Lookup(binary, runnableID string) (Runnable, bool) {
	for _, r := range registry[binary] {
		if r.ID == runnableID {
			return r, true
		}
	}
	return Runnable{}, false
}

// Known reports whether binary is in the registry (used to reject unknown binaries
// at image registration — see sp-9xr.3).
func Known(binary string) bool {
	_, ok := registry[binary]
	return ok
}
