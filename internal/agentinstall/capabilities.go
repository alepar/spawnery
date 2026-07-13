package agentinstall

// CapabilityEntry is a single (kind, agent) -> status row in the capabilities matrix.
type CapabilityEntry struct {
	Kind   Kind             `json:"kind"`
	Agent  string           `json:"agent"`
	Status CapabilityStatus `json:"status"`
}

// capabilityKinds is the matrix column order.
// "instructions" is last; it is not a routable Kind constant but appears in the matrix.
var capabilityKinds = []Kind{KindSkill, KindMCP, KindConfig, KindPlugin, Kind("instructions")}

// Capabilities returns the (kind,agent)->status matrix in canonical agent x kind order.
// Every registered emitter (claude, codex, opencode, hermes, goose, pi) has its own
// Capabilities() override reflecting spike-verified truth (sp-mwco.2.2/2.5); only an
// emitter with no override at all would fall back to baseEmitter's all-no-op default.
func Capabilities(reg Registry) []CapabilityEntry {
	var out []CapabilityEntry
	for _, name := range reg.Names() {
		e, _ := reg.Lookup(name)
		caps := e.Capabilities()
		for _, k := range capabilityKinds {
			st, ok := caps[k]
			if !ok {
				st = CapStatusNoOp
			}
			out = append(out, CapabilityEntry{Kind: k, Agent: name, Status: st})
		}
	}
	return out
}
