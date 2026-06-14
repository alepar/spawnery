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
	Launch []string // reference-only: canonical argv to start the agent; the image's dispatcher entrypoint owns actual launch (sp-9xr.13b). Retained for self-documentation and invariant checks (tmux runnables must declare one).
	Resume []string // argv to resume a conversation; nil = none wired yet (see sp-9xr.10)
	Relay  Relay    // which relay carries this runnable's bytes
	Label  string   // human-readable, shown in the runnable dropdown
}

// registry is the single source of truth: binary name -> its runnables.
// Launch argvs are reference-only documentation (the image dispatcher owns actual launch,
// sp-9xr.13b); Resume argvs are validated when each resume path is wired (sp-9xr.10).
var registry = map[string][]Runnable{
	"goose": {
		{ID: "goose-acp", Mode: ModeACP, Launch: []string{"goose", "acp"}, Relay: RelayPump, Label: "Goose · rich web"},
		{ID: "goose-tui", Mode: ModeTmux, Launch: []string{"goose", "session"}, Relay: RelayRawPTY, Label: "Goose · terminal"},
	},
	"opencode": {
		{ID: "opencode-served", Mode: ModeServed, Launch: []string{"opencode", "serve", "--port", "4096", "--hostname", "127.0.0.1"}, Relay: RelayOcadapter, Label: "opencode"},
		{ID: "opencode-tui", Mode: ModeTmux, Launch: []string{"opencode"}, Relay: RelayRawPTY, Label: "opencode · terminal"},
	},
	"claude-code": {
		{ID: "claude-tui", Mode: ModeTmux, Launch: []string{"claude"}, Relay: RelayRawPTY, Label: "Claude Code · terminal"},
	},
	"codex": {
		{ID: "codex-tui", Mode: ModeTmux, Launch: []string{"codex"}, Relay: RelayRawPTY, Label: "Codex · terminal"},
	},
	"hermes": {
		{ID: "hermes-acp", Mode: ModeACP, Launch: []string{"hermes", "acp"}, Relay: RelayPump, Label: "Hermes · rich web"},
	},
	// NOTE: the "stub" TEST FIXTURE binary (cmd/stubagent) is registered only under the
	// e2e_fixtures build tag — see agentcaps_e2e.go. RELEASE binaries exclude it.
}

// Runnables returns the runnables for a known binary. ok is false for an unknown
// binary. The returned slice is a copy, so callers cannot mutate the registry by
// reassigning elements; the inner Launch/Resume slices are shared, so treat them as
// read-only.
func Runnables(binary string) ([]Runnable, bool) {
	src, ok := registry[binary]
	if !ok {
		return nil, false
	}
	return append([]Runnable(nil), src...), true
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

// FindRunnable resolves a runnable by its id across all binaries. Runnable ids are globally
// unique (see TestRunnableIDsGloballyUnique), so the first match is unambiguous.
func FindRunnable(id string) (Runnable, bool) {
	for _, rs := range registry {
		for _, r := range rs {
			if r.ID == id {
				return r, true
			}
		}
	}
	return Runnable{}, false
}
