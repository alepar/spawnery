package agentcaps

// binaryEmitter maps an agent BINARY (agentcaps vocabulary, i.e. a registry key) to its
// agentinstall EMITTER name (an internal/agentinstall.Registry key — see
// internal/agentinstall/registry.go). This is the single place that reconciles the two
// vocabularies; "claude-code" -> "claude" is the only name that differs.
//
// A binary absent from this map has no emitter (nothing to install for it) — that is either
// because it is listed in noEmitterBinaries below, or because EmitterForBinary/EmitterForRunnable
// simply report ok=false for it. TestEveryBinaryHasEmitterEntry guards that every REGISTERED
// binary is accounted for one way or the other, so a newly added agent cannot silently become a
// skills no-op.
var binaryEmitter = map[string]string{
	"claude-code": "claude", // the ONLY name that differs
	"codex":       "codex",
	"opencode":    "opencode",
	"goose":       "goose",
	"hermes":      "hermes",
	"pi":          "pi", // emitter registered by sp-mwco.2.5
}

// noEmitterBinaries lists registry binaries that deliberately have no agentinstall emitter.
var noEmitterBinaries = map[string]bool{
	// stub is the e2e_fixtures test-only echo agent (cmd/stubagent) — it has no skills/config
	// story and is never present in a release image's registry.
	"stub": true,
}

// EmitterForBinary maps an agent binary (agentcaps vocabulary) to its agentinstall emitter
// name. ok is false for an unknown binary or one with no emitter (see noEmitterBinaries).
func EmitterForBinary(binary string) (string, bool) {
	emitter, ok := binaryEmitter[binary]
	return emitter, ok
}

// EmitterForRunnable resolves a runnable ID (e.g. "goose-tui") to its agentinstall emitter
// name, by first finding the binary that owns the runnable, then mapping that binary to its
// emitter. ok is false when the runnable is unknown or its binary has no emitter.
//
// Runnable does not carry its owning binary name (see agentcaps.go), so this walks the
// registry directly rather than reusing FindRunnable.
func EmitterForRunnable(runnableID string) (string, bool) {
	for binary, rs := range registry {
		for _, r := range rs {
			if r.ID == runnableID {
				return EmitterForBinary(binary)
			}
		}
	}
	return "", false
}

// AllRunnableIDs returns every runnable ID across all binaries currently registered (order
// unspecified). It exists for cross-registry reconciliation / drift tests (see
// reconcile_test.go) so those tests discover runnables live from the registry instead of
// hardcoding a list that could itself drift.
func AllRunnableIDs() []string {
	var ids []string
	for _, rs := range registry {
		for _, r := range rs {
			ids = append(ids, r.ID)
		}
	}
	return ids
}
