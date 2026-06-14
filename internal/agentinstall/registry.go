package agentinstall

// Registry is a map from normalized agent name to its Emitter.
type Registry map[string]Emitter

// NewRegistry creates and returns a Registry pre-populated with all supported agents.
// The paths are resolved from the provided Environ (for test hermeticity).
func NewRegistry(env Environ) Registry {
	homeDir := env.Home()
	xdgConfig := env.XDGConfigHome()

	codexHome := env.CodexHome()
	if codexHome == "" {
		codexHome = homeDir + "/.codex"
	}

	r := make(Registry)
	r["claude"] = newClaudeEmitter(homeDir)
	r["codex"] = newCodexEmitter(codexHome)
	r["opencode"] = newOpencodeEmitter(xdgConfig)
	r["hermes"] = newHermesEmitter(homeDir)
	r["goose"] = newGooseEmitter(xdgConfig)
	return r
}

// Lookup returns the Emitter for the given agent name, and whether it was found.
func (r Registry) Lookup(name string) (Emitter, bool) {
	e, ok := r[name]
	return e, ok
}

// Names returns the list of registered agent names in a deterministic order.
func (r Registry) Names() []string {
	// Return in canonical order.
	canonical := []string{"claude", "codex", "opencode", "hermes", "goose"}
	out := make([]string, 0, len(canonical))
	for _, name := range canonical {
		if _, ok := r[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// Layouts returns all AgentLayout values in canonical order.
func (r Registry) Layouts() []AgentLayout {
	names := r.Names()
	out := make([]AgentLayout, 0, len(names))
	for _, name := range names {
		out = append(out, r[name].Layout())
	}
	return out
}
