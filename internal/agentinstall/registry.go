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
	// pi (sp-mwco.2.5, spike-confirmed: reads ~/.agents/skills natively, no glue).
	//
	// MIGRATION HAZARD, decided (not a surprise): registering pi here silently widens
	// every existing profile entry with targets: []/["all"] to a sixth agent. This is
	// intended, not grandfathered — "all" is stored as ["all"], translated to
	// "all-detected" at CP assembly time, and resolved in the pod against the agents
	// actually detected there (Detect ∩ registry); a new registry entry joining is
	// exactly what "every agent this spawn could run" asks for. Blast radius is
	// bounded: a pi spawn gets the same skills the profile already gives
	// claude/goose/opencode; explicit target lists are unaffected; the web "all"
	// checkbox renders from the generated AGENTS list, so pi shows up as a checkbox
	// the user can uncheck. See the dated Post-Implementation Note in
	// docs/superpowers/specs/2026-07-12-all-agent-skill-install-design.md.
	r["pi"] = newPiEmitter(homeDir)
	// nori is deliberately NOT registered: it is an ACP client, not a harness — it has
	// no native skill/config surface of its own to install into.
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
	canonical := []string{"claude", "codex", "opencode", "hermes", "goose", "pi"}
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
