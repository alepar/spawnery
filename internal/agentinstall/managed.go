package agentinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ManagedEntry records provenance for a single installed artifact component.
type ManagedEntry struct {
	Kind           Kind   `json:"kind"`
	Agent          string `json:"agent"`
	Name           string `json:"name"`
	NativePath     string `json:"native_path"`
	NativeKey      string `json:"native_key,omitempty"`
	File           string `json:"file,omitempty"`
	ProfileID      string `json:"profile_id,omitempty"`
	ProfileVersion string `json:"profile_version,omitempty"`
	// SourceURL/SourceCommit record a skill's untrusted-external provenance (sp-mwco.2.8 §4.6),
	// so ~/.spawnery/managed.json shows where each installed skill came from. Empty for
	// non-skill kinds and for operator-authored (no-Source) skills.
	SourceURL    string `json:"source_url,omitempty"`
	SourceCommit string `json:"source_commit,omitempty"`
}

// ManagedIndex is the structured content of ~/.spawnery/managed.json.
type ManagedIndex struct {
	Entries []ManagedEntry `json:"entries"`
}

// provenanceEntries derives managed.json entries for one APPLIED (artifact × agent) pair.
// It should only be called for StatusApplied reports.
func provenanceEntries(layout AgentLayout, a Artifact, opts Options) []ManagedEntry {
	base := func(kind Kind) ManagedEntry {
		return ManagedEntry{
			Kind:           kind,
			Agent:          layout.Name,
			Name:           a.Name,
			ProfileID:      opts.ProfileID,
			ProfileVersion: opts.ProfileVersion,
		}
	}

	var out []ManagedEntry
	switch a.Kind {
	case KindSkill:
		// Every applied skill has a canonical row; agents with a real native copy
		// (layout.SkillPath != "") additionally get a distinct-NativeKey row so
		// WriteManagedIndex's upsert key doesn't collide the two.
		canonical := base(KindSkill)
		canonical.NativeKey = "skills.canonical"
		canonical.File = filepath.Join(CanonicalSkillsDir(opts.HomeDir), a.Name)
		canonical.NativePath = canonical.File
		if a.Skill != nil && a.Skill.Source != nil {
			canonical.SourceURL = a.Skill.Source.URL
			canonical.SourceCommit = a.Skill.Source.Commit
		}
		out = append(out, canonical)

		if layout.SkillPath != "" {
			native := base(KindSkill)
			native.NativeKey = "skills." + layout.Name
			native.File = filepath.Join(layout.SkillPath, a.Name)
			native.NativePath = native.File
			if a.Skill != nil && a.Skill.Source != nil {
				native.SourceURL = a.Skill.Source.URL
				native.SourceCommit = a.Skill.Source.Commit
			}
			out = append(out, native)
		}

	case KindMCP:
		e := base(KindMCP)
		e.NativePath = layout.MCPPath
		e.NativeKey = mcpProvenanceKey(layout.Name, a.Name)
		out = append(out, e)

	case KindPlugin:
		e := base(KindPlugin)
		e.NativePath = layout.ConfigPath
		e.NativeKey = pluginProvenanceKey(layout.Name, a.Plugin)
		out = append(out, e)

	case KindConfig:
		if a.Config != nil && (len(a.Config.Normalized) > 0 || len(a.Config.Native) > 0) {
			e := base(KindConfig)
			e.NativePath = layout.ConfigPath
			out = append(out, e)
		}
		if a.Config != nil && a.Config.Instructions != "" && layout.InstructionsPath != "" {
			e := base(Kind("instructions"))
			e.NativePath = layout.InstructionsPath
			e.File = layout.InstructionsPath
			out = append(out, e)
		}
	}

	return out
}

// mcpProvenanceKey returns the native key used for MCP entries in the given agent's config.
func mcpProvenanceKey(agent, name string) string {
	switch agent {
	case "codex":
		return "mcp_servers." + name
	case "opencode":
		return "mcp." + name
	default: // claude
		return "mcpServers." + name
	}
}

// pluginProvenanceKey returns the native key used for plugin entries in the given agent's config.
func pluginProvenanceKey(agent string, p *PluginPayload) string {
	if p == nil {
		return ""
	}
	switch agent {
	case "opencode":
		return "plugin"
	case "codex":
		return "plugins." + p.Plugin + "@" + p.Marketplace
	default: // claude
		return "enabledPlugins." + p.Plugin + "@" + p.Marketplace
	}
}

// WriteManagedIndex upserts entries (keyed by kind+agent+name+native_key) into the index at path.
// The directory containing path is created if needed. A no-op when path is empty or entries is empty.
func WriteManagedIndex(path string, entries []ManagedEntry) error {
	if path == "" || len(entries) == 0 {
		return nil
	}

	var ix ManagedIndex
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &ix)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read managed index %s: %w", path, err)
	}

	entryKey := func(e ManagedEntry) string {
		return string(e.Kind) + "|" + e.Agent + "|" + e.Name + "|" + e.NativeKey
	}
	pos := make(map[string]int, len(ix.Entries))
	for i, e := range ix.Entries {
		pos[entryKey(e)] = i
	}

	for _, e := range entries {
		k := entryKey(e)
		if i, ok := pos[k]; ok {
			ix.Entries[i] = e
		} else {
			pos[k] = len(ix.Entries)
			ix.Entries = append(ix.Entries, e)
		}
	}

	data, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal managed index: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create index dir: %w", err)
	}
	return atomicWriteFile(path, data, 0o600)
}
