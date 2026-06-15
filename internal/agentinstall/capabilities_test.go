package agentinstall_test

import (
	"testing"

	"spawnery/internal/agentinstall"
)

func TestCapabilitiesMatrix(t *testing.T) {
	reg := agentinstall.NewRegistry(agentinstall.MapEnviron{"HOME": "/h"})
	entries := agentinstall.Capabilities(reg)
	idx := map[string]agentinstall.CapabilityStatus{}
	for _, e := range entries {
		idx[string(e.Kind)+"/"+e.Agent] = e.Status
	}
	want := map[string]agentinstall.CapabilityStatus{
		"skill/claude":        "supported",
		"mcp/claude":          "supported",
		"config/claude":       "supported",
		"plugin/claude":       "supported",
		"instructions/claude": "supported",
		"skill/opencode":      "no-op",
		"plugin/opencode":     "best-effort",
		"instructions/opencode": "supported",
		"mcp/hermes":          "no-op",
		"config/goose":        "no-op",
		"skill/codex":         "supported",
	}
	for k, v := range want {
		if idx[k] != v {
			t.Errorf("%s = %q want %q", k, idx[k], v)
		}
	}
}
