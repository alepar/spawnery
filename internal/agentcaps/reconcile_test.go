package agentcaps_test

// Cross-registry drift test: agentcaps owns the runnable/binary -> emitter vocabulary;
// internal/agentinstall owns the emitter registry itself. This file is the acceptance test
// that the two agree in both directions — see sp-mwco.2.6.

import (
	"testing"

	"spawnery/internal/agentcaps"
	"spawnery/internal/agentinstall"
)

// TestForwardEveryMappedEmitterIsRegistered asserts: for every runnable agentcaps knows about,
// the emitter it maps to (if any) is actually registered in agentinstall.NewRegistry. Catches
// agentcaps claiming an emitter that agentinstall doesn't have.
func TestForwardEveryMappedEmitterIsRegistered(t *testing.T) {
	reg := agentinstall.NewRegistry(agentinstall.OSEnviron{})

	for _, id := range agentcaps.AllRunnableIDs() {
		emitter, ok := agentcaps.EmitterForRunnable(id)
		if !ok {
			// Emitter-less runnable (e.g. the stub-acp test fixture) — nothing to check.
			continue
		}
		if _, ok := reg.Lookup(emitter); !ok {
			t.Errorf("runnable %q maps to emitter %q, but agentinstall.NewRegistry does not register it", id, emitter)
		}
	}
}

// TestReverseEveryRegisteredEmitterIsReachable asserts: for every emitter name
// agentinstall.NewRegistry registers, at least one agentcaps runnable maps to it. This is the
// strict direction — it catches agentinstall registering an emitter with no corresponding
// agentcaps entry (a renamed key, or a registered-but-unreachable emitter).
func TestReverseEveryRegisteredEmitterIsReachable(t *testing.T) {
	reg := agentinstall.NewRegistry(agentinstall.OSEnviron{})

	reachable := map[string]bool{}
	for _, id := range agentcaps.AllRunnableIDs() {
		if emitter, ok := agentcaps.EmitterForRunnable(id); ok {
			reachable[emitter] = true
		}
	}

	for _, name := range reg.Names() {
		if !reachable[name] {
			t.Errorf("agentinstall registers emitter %q, but no agentcaps runnable maps to it via EmitterForRunnable", name)
		}
	}
}
