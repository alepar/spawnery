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

// enforceProfileArtifactCap returns a CodeInvalidArgument error when existingExpanded+adding
// would exceed maxArtifactsPerSpawn-1 (63; the manifest takes the 64th slot — sp-mwco.1.8/1.12
// §4.4). existingExpanded is the profile's current EXPANDED artifact count (1 per catalog_ref/
// custom entry, member-count-minus-excludes per bundle_ref entry); adding is the new entry's own
// expanded member count (1 for catalog_ref/custom, the post-exclude member count for bundle_ref).
// EVERY entry source is accounted the same way, so no combination of attaches can build a profile
// past what assembly (validateAndMergeArtifacts) will accept. Assembly stays the authoritative
// enforcement point; this is attach-time UX so an over-budget profile is rejected immediately
// instead of at CreateSpawn.
func enforceProfileArtifactCap(existingExpanded, adding int) error {
	budget := maxArtifactsPerSpawn - 1
	if existingExpanded+adding > budget {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("profile would hold %d skill artifacts (adding %d), exceeding the maximum %d per spawn (manifest.json takes the remaining slot)",
				existingExpanded+adding, adding, budget))
	}
	return nil
}
