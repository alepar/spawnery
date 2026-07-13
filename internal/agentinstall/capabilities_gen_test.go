package agentinstall_test

import (
	"encoding/json"
	"os"
	"testing"

	"spawnery/internal/agentinstall"
)

// genCapabilitiesPath is the web-side generated export, relative to this package dir.
const genCapabilitiesPath = "../../web/src/api/capabilities.gen.json"

// regenCommand is the exact command that regenerates genCapabilitiesPath — kept in sync
// with the header comment in web/src/api/capabilities.ts.
const regenCommand = `distrobox enter --root dev-spawnery -- bash -lc 'cd <wt> && go run ./cmd/agentinstall list-agents --capabilities > web/src/api/capabilities.gen.json'`

// TestCapabilitiesGenNotStale is the drift guard the epic lacked: web/src/api/
// capabilities.gen.json must exactly match (kind, agent, status) — order included — what
// the Go source of truth (agentinstall.Capabilities) computes right now. A failure here
// means the matrix changed in Go without the export being regenerated.
func TestCapabilitiesGenNotStale(t *testing.T) {
	data, err := os.ReadFile(genCapabilitiesPath)
	if err != nil {
		t.Fatalf("read %s: %v\nregenerate with:\n  %s", genCapabilitiesPath, err, regenCommand)
	}

	var got []agentinstall.CapabilityEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse %s: %v", genCapabilitiesPath, err)
	}

	reg := agentinstall.NewRegistry(agentinstall.MapEnviron{"HOME": "/h"})
	want := agentinstall.Capabilities(reg)

	if len(got) != len(want) {
		t.Fatalf("%s is stale: got %d entries, want %d\nregenerate with:\n  %s", genCapabilitiesPath, len(got), len(want), regenCommand)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s is stale at entry %d: got %+v want %+v\nregenerate with:\n  %s", genCapabilitiesPath, i, got[i], want[i], regenCommand)
		}
	}
}
