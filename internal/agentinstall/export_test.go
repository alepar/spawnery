// Package agentinstall — test export file.
// Exposes internal execpolicy symbols for the external agentinstall_test package.
// This file is only compiled during testing.
package agentinstall

// ExportCodexExecPolicyVersion exposes the pinned rules-format version for tests.
const ExportCodexExecPolicyVersion = codexExecPolicyVersion

// ExportCommandPolicy constructs a commandPolicy from explicit slices (test helper).
func ExportCommandPolicy(allowed, denied []string) commandPolicy {
	return commandPolicy{Allowed: allowed, Denied: denied}
}

// ExportRenderCodexRules exposes renderCodexRules for tests.
func ExportRenderCodexRules(cp commandPolicy) []byte {
	return renderCodexRules(cp)
}

// ExportWriteCodexExecPolicy exposes writeCodexExecPolicy for tests.
func ExportWriteCodexExecPolicy(rulesDir string, cp commandPolicy) (string, error) {
	return writeCodexExecPolicy(rulesDir, cp)
}
