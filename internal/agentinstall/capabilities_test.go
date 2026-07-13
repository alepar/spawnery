package agentinstall_test

import (
	"testing"

	"spawnery/internal/agentinstall"
)

// TestCapabilitiesMatrix pins the full 6x5 capability matrix to spike-verified truth
// (sp-mwco.2.2/2.5, spec §4.1/§4.3). Any drift here must be a deliberate capability
// decision, not an accident.
func TestCapabilitiesMatrix(t *testing.T) {
	reg := agentinstall.NewRegistry(agentinstall.MapEnviron{"HOME": "/h"})
	entries := agentinstall.Capabilities(reg)
	idx := map[string]agentinstall.CapabilityStatus{}
	for _, e := range entries {
		idx[string(e.Kind)+"/"+e.Agent] = e.Status
	}
	want := map[string]agentinstall.CapabilityStatus{
		// claude: everything fully supported.
		"skill/claude":        "supported",
		"mcp/claude":          "supported",
		"config/claude":       "supported",
		"plugin/claude":       "supported",
		"instructions/claude": "supported",

		// codex: skill=best-effort (writes both trees; read side unverified, sp-9e6q),
		// everything else supported.
		"skill/codex":        "best-effort",
		"mcp/codex":          "supported",
		"config/codex":       "supported",
		"plugin/codex":       "supported",
		"instructions/codex": "supported",

		// opencode: skill=supported (spike-confirmed native read), mcp/config/instructions
		// supported, plugin=best-effort.
		"skill/opencode":        "supported",
		"mcp/opencode":          "supported",
		"config/opencode":       "supported",
		"plugin/opencode":       "best-effort",
		"instructions/opencode": "supported",

		// goose: skill=supported (native, unconditional read), everything else no-op.
		"skill/goose":        "supported",
		"mcp/goose":          "no-op",
		"config/goose":       "no-op",
		"plugin/goose":       "no-op",
		"instructions/goose": "no-op",

		// hermes: skill=supported (we emit the external_dirs glue), everything else no-op.
		"skill/hermes":        "supported",
		"mcp/hermes":          "no-op",
		"config/hermes":       "no-op",
		"plugin/hermes":       "no-op",
		"instructions/hermes": "no-op",

		// pi: skill=supported (native, no glue), everything else no-op.
		"skill/pi":        "supported",
		"mcp/pi":          "no-op",
		"config/pi":       "no-op",
		"plugin/pi":       "no-op",
		"instructions/pi": "no-op",
	}
	if len(idx) != len(want) {
		t.Fatalf("Capabilities() entry count = %d, want %d (idx=%v)", len(idx), len(want), idx)
	}
	for k, v := range want {
		if idx[k] != v {
			t.Errorf("%s = %q want %q", k, idx[k], v)
		}
	}
}
