package agentcaps

import "testing"

func TestRunnablesKnownBinary(t *testing.T) {
	rs, ok := Runnables("goose")
	if !ok {
		t.Fatalf("goose should be a known binary")
	}
	if len(rs) != 2 {
		t.Fatalf("want 2 goose runnables, got %d", len(rs))
	}
	byID := map[string]Runnable{}
	for _, r := range rs {
		byID[r.ID] = r
	}
	if byID["goose-acp"].Mode != ModeACP {
		t.Fatalf("goose-acp mode = %q, want %q", byID["goose-acp"].Mode, ModeACP)
	}
	if byID["goose-tui"].Mode != ModeTmux {
		t.Fatalf("goose-tui mode = %q, want %q", byID["goose-tui"].Mode, ModeTmux)
	}
}

func TestRunnablesUnknownBinary(t *testing.T) {
	if _, ok := Runnables("does-not-exist"); ok {
		t.Fatalf("unknown binary should not be known")
	}
}

func TestLookup(t *testing.T) {
	r, ok := Lookup("goose", "goose-tui")
	if !ok {
		t.Fatalf("goose/goose-tui should resolve")
	}
	if r.Mode != ModeTmux || r.Relay != RelayRawPTY {
		t.Fatalf("goose-tui = mode %q relay %q, want %q / %q", r.Mode, r.Relay, ModeTmux, RelayRawPTY)
	}
	if len(r.Launch) == 0 {
		t.Fatalf("tmux runnable goose-tui must have a Launch argv")
	}
	if _, ok := Lookup("goose", "nope"); ok {
		t.Fatalf("unknown runnable id should not resolve")
	}
	if _, ok := Lookup("nope", "goose-tui"); ok {
		t.Fatalf("unknown binary should not resolve")
	}
}

func TestKnown(t *testing.T) {
	for _, b := range []string{"goose", "opencode", "claude-code"} {
		if !Known(b) {
			t.Fatalf("%q should be known", b)
		}
	}
	if Known("aider") {
		t.Fatalf("aider is not seeded yet; should not be known")
	}
}

func TestFindRunnable(t *testing.T) {
	r, ok := FindRunnable("goose-tui")
	if !ok || r.Mode != ModeTmux {
		t.Fatalf("FindRunnable(goose-tui) = %+v ok=%v", r, ok)
	}
	r, ok = FindRunnable("opencode-served")
	if !ok || r.Mode != ModeServed || len(r.Launch) == 0 {
		t.Fatalf("FindRunnable(opencode-served) = %+v ok=%v", r, ok)
	}
	if _, ok := FindRunnable("does-not-exist"); ok {
		t.Fatalf("unknown id should not resolve")
	}
}

// FindRunnable relies on runnable IDs being globally unique across binaries.
func TestRunnableIDsGloballyUnique(t *testing.T) {
	seen := map[string]string{}
	for binary, rs := range registry {
		for _, r := range rs {
			if other, dup := seen[r.ID]; dup {
				t.Fatalf("runnable id %q is in both %q and %q", r.ID, other, binary)
			}
			seen[r.ID] = binary
		}
	}
}

func TestOpencodeTuiRunnable(t *testing.T) {
	r, ok := Lookup("opencode", "opencode-tui")
	if !ok {
		t.Fatalf("opencode/opencode-tui should resolve")
	}
	if r.Mode != ModeTmux || r.Relay != RelayRawPTY {
		t.Fatalf("opencode-tui = mode %q relay %q", r.Mode, r.Relay)
	}
	if len(r.Launch) == 0 {
		t.Fatalf("opencode-tui needs a Launch argv")
	}
}

func TestRegistryInvariants(t *testing.T) {
	validMode := map[Mode]bool{ModeACP: true, ModeTmux: true, ModeServed: true}
	validRelay := map[Relay]bool{RelayPump: true, RelayOcadapter: true, RelayRawPTY: true}

	for binary, rs := range registry {
		if binary == "" {
			t.Fatalf("registry has an empty binary key")
		}
		seen := map[string]bool{}
		for _, r := range rs {
			if r.ID == "" {
				t.Fatalf("%s: runnable with empty ID", binary)
			}
			if seen[r.ID] {
				t.Fatalf("%s: duplicate runnable ID %q", binary, r.ID)
			}
			seen[r.ID] = true
			if r.Label == "" {
				t.Fatalf("%s/%s: empty Label", binary, r.ID)
			}
			if !validMode[r.Mode] {
				t.Fatalf("%s/%s: invalid Mode %q", binary, r.ID, r.Mode)
			}
			if !validRelay[r.Relay] {
				t.Fatalf("%s/%s: invalid Relay %q", binary, r.ID, r.Relay)
			}
			if r.Mode == ModeTmux && len(r.Launch) == 0 {
				t.Fatalf("%s/%s: tmux runnable must have a Launch argv", binary, r.ID)
			}
		}
	}
}

func TestStubAcpRunnableRegistered(t *testing.T) {
	rs, ok := Runnables("stub")
	if !ok || len(rs) != 1 {
		t.Fatalf("Runnables(\"stub\") = %v, %v; want one runnable", rs, ok)
	}
	if rs[0].ID != "stub-acp" || rs[0].Mode != ModeACP || rs[0].Relay != RelayPump {
		t.Fatalf("stub runnable = %+v; want id=stub-acp mode=acp relay=pump", rs[0])
	}
}
