package agentcaps

import "testing"

func TestEmitterForRunnable(t *testing.T) {
	cases := []struct {
		runnable string
		want     string
		wantOK   bool
	}{
		{"claude-tui", "claude", true},
		{"codex-tui", "codex", true},
		{"opencode-served", "opencode", true},
		{"opencode-tui", "opencode", true},
		{"goose-acp", "goose", true},
		{"goose-tui", "goose", true},
		{"hermes-acp", "hermes", true},
		{"pi-tui", "pi", true},
		{"pi-acp", "pi", true},
		{"shell", "", false},
		{"nori", "", false},
		{"", "", false},
		{"clade-tui", "", false},
	}
	for _, tc := range cases {
		got, ok := EmitterForRunnable(tc.runnable)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("EmitterForRunnable(%q) = (%q, %v), want (%q, %v)", tc.runnable, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestEmitterForBinary(t *testing.T) {
	cases := []struct {
		binary string
		want   string
		wantOK bool
	}{
		{"claude-code", "claude", true},
		{"codex", "codex", true},
		{"opencode", "opencode", true},
		{"goose", "goose", true},
		{"hermes", "hermes", true},
		{"pi", "pi", true},
		{"stub", "", false},
		{"does-not-exist", "", false},
	}
	for _, tc := range cases {
		got, ok := EmitterForBinary(tc.binary)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("EmitterForBinary(%q) = (%q, %v), want (%q, %v)", tc.binary, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestEveryBinaryHasEmitterEntry is the drift guard: every binary registered in the
// agentcaps registry (see Known/Runnables) must have a binaryEmitter entry, unless it is
// explicitly allowlisted as emitter-less in noEmitterBinaries. This is what stops a future
// agent added to agentcaps from silently becoming a skills no-op.
func TestEveryBinaryHasEmitterEntry(t *testing.T) {
	for binary := range registry {
		if noEmitterBinaries[binary] {
			continue
		}
		if _, ok := binaryEmitter[binary]; !ok {
			t.Errorf("binary %q is registered in agentcaps but has no binaryEmitter entry (add one, or add it to noEmitterBinaries with a comment explaining why it has no emitter)", binary)
		}
	}
}
