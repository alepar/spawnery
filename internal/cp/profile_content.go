package cp

import (
	"fmt"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	"spawnery/internal/cp/store"
)

// validateContentName returns an error if name is not a clean single path segment:
// non-empty, no path separators, not "." or "..", filepath.Clean(name)==name.
// This replicates the agentinstall.validateSkillName rule (do not cross-import internals).
func validateContentName(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("name must not contain path separators: %q", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("name must not be %q", name)
	}
	if filepath.Clean(name) != name {
		return fmt.Errorf("name is not a clean single path segment: %q", name)
	}
	return nil
}

// validateCustomContent validates a custom (user-supplied or curated) content item before storage.
// Rules:
//   - name: non-empty, clean single path segment (no path separators, not "." or "..").
//   - name must pass confineDestPath (no absolute path, no ".." escape).
//   - inline bytes must be non-empty.
//   - inline bytes must not exceed maxArtifactInlineBytes (1 MiB).
//   - Light per-kind shape check (MVP): mcp/config/plugin content must be non-empty;
//     well-formedness deep-check is deferred to assembly (sp-nrzf.3.7).
//
// Returns a Connect CodeInvalidArgument error on any violation.
func validateCustomContent(kind store.ProfileEntryKind, name string, inline []byte) error {
	if err := validateContentName(name); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	// Path confinement: name as a dest path must not be absolute or escape its root.
	if err := confineDestPath(name); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name: %w", err))
	}
	if len(inline) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("content must not be empty"))
	}
	if len(inline) > maxArtifactInlineBytes {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("content %d bytes exceeds maximum %d", len(inline), maxArtifactInlineBytes))
	}
	// Light per-kind shape check (MVP): all supported kinds require non-empty content.
	// Deep well-formedness parsing (JSON structure for MCP/config, tar for plugin/skill) is
	// deferred to the assembly layer (sp-nrzf.3.7).
	switch kind {
	case store.ProfileEntrySkill, store.ProfileEntryMCP, store.ProfileEntryConfig, store.ProfileEntryPlugin:
		// non-empty content already validated above; no deep parse here.
	default:
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unsupported kind: %q", kind))
	}
	return nil
}

// enforceProfileEntryCap returns a CodeInvalidArgument error when the current entry count
// would exceed maxArtifactsPerSpawn (64). existing is the count before adding the new entry.
func enforceProfileEntryCap(existing int) error {
	if existing >= maxArtifactsPerSpawn {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("profile entry count %d would exceed maximum %d", existing+1, maxArtifactsPerSpawn))
	}
	return nil
}
