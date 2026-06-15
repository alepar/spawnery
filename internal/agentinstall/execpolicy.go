package agentinstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// codexExecPolicyVersion pins the rules file format version.  Bump this if the
// format ever changes in a backwards-incompatible way so consumers can detect
// stale files.
const codexExecPolicyVersion = "1"

// commandPolicy holds the allow/deny command patterns for codex execpolicy.
// Patterns are arbitrary shell globs or command prefixes accepted by codex.
type commandPolicy struct {
	Allowed []string
	Denied  []string
}

// empty reports whether both lists are empty.
func (cp commandPolicy) empty() bool {
	return len(cp.Allowed) == 0 && len(cp.Denied) == 0
}

// renderCodexRules returns the text content of a codex rules file for the given
// commandPolicy.  The format is pinned to codexExecPolicyVersion:
//
//	# codex exec policy rules (format version <V>)
//	allow <pattern>
//	deny  <pattern>
//
// Patterns appear in input order: allowed first, denied second.  The output is
// deterministic across multiple calls with the same input.
func renderCodexRules(cp commandPolicy) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# codex exec policy rules (format version %s)\n", codexExecPolicyVersion)
	for _, pat := range cp.Allowed {
		fmt.Fprintf(&sb, "allow %s\n", pat)
	}
	for _, pat := range cp.Denied {
		fmt.Fprintf(&sb, "deny %s\n", pat)
	}
	return []byte(sb.String())
}

// writeCodexExecPolicy atomically writes a codex rules file at
// <rulesDir>/default.rules, creating rulesDir (and parents) if absent.
// Returns the path of the written file.
func writeCodexExecPolicy(rulesDir string, cp commandPolicy) (string, error) {
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", rulesDir, err)
	}
	path := filepath.Join(rulesDir, "default.rules")
	if err := atomicWriteFile(path, renderCodexRules(cp), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
