// Package spec is a stdlib-only leaf package holding the canonical artifact
// model (the engine INPUT contract) shared by the agentinstall CLI and the
// spawnery control plane. It imports ONLY the Go standard library — never
// spawnery/internal/*, spawnery/gen/*, or any third-party module (go-toml,
// hujson, etc.) — so the control plane can import it without pulling the
// agentinstall emitter dependency tree. Enforced by TestSpecLeafInvariant.
package spec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CurrentSchemaVersion is the highest manifest schema_version this build
// understands. A manifest declaring a version greater than this is rejected
// as a hard error (a newer producer than this consumer — major incompat).
const CurrentSchemaVersion = 1

// Kind is the logical artifact kind.
type Kind string

const (
	KindSkill  Kind = "skill"
	KindMCP    Kind = "mcp"
	KindConfig Kind = "config"
	KindPlugin Kind = "plugin"
)

// SkillPayload is the skill artifact payload.
type SkillPayload struct {
	// Dir is the path within the staging payloads directory to the skill directory tree.
	Dir string `json:"dir"`
}

// MCPTransportStdio is a stdio MCP transport configuration.
type MCPTransportStdio struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// MCPTransportHTTP is an HTTP MCP transport configuration.
type MCPTransportHTTP struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// MCPPayload is the MCP artifact payload.
type MCPPayload struct {
	// Exactly one of Stdio or HTTP must be set.
	Stdio      *MCPTransportStdio `json:"stdio,omitempty"`
	HTTP       *MCPTransportHTTP  `json:"http,omitempty"`
	SecretRefs []string           `json:"secretRefs,omitempty"`
}

// ConfigPayload is the config artifact payload.
type ConfigPayload struct {
	// Normalized is the canonical key map (launcher-managed keys like "model" are forbidden).
	Normalized map[string]interface{} `json:"normalized,omitempty"`
	// Native is agent-specific passthrough fragments keyed by agent name.
	Native map[string]interface{} `json:"native,omitempty"`
	// Instructions is a content blob written to a DEDICATED per-agent managed file
	// (replace-on-apply); never appended to CLAUDE.md/AGENTS.md memory.
	Instructions string `json:"instructions,omitempty"`
}

// PluginPayload is the plugin artifact payload (local / image-baked install).
type PluginPayload struct {
	// Plugin is the plugin name (identity within a marketplace).
	Plugin string `json:"plugin"`
	// Marketplace is the marketplace name; the enable key is "<Plugin>@<Marketplace>".
	Marketplace string `json:"marketplace"`
	// Source is the local/baked marketplace source (path); claude+codex.
	Source string `json:"source,omitempty"`
	// LocalFile is an opencode local plugin file path (preferred, offline).
	LocalFile string `json:"localFile,omitempty"`
	// NPM is an opencode npm module spec (best-effort: needs registry egress at cold start).
	NPM string `json:"npm,omitempty"`
	// RequiresOAuth marks a codex plugin whose marketplace implies an OAuth ON_INSTALL
	// app/MCP — cannot complete headless, so codex no-ops + reports.
	RequiresOAuth bool `json:"requiresOAuth,omitempty"`
}

// Artifact is the canonical descriptor of a single logical artifact.
// Targets is either a list of agent names (claude|codex|opencode|hermes|goose)
// or the string "all-detected".
type Artifact struct {
	Kind    Kind     `json:"kind"`
	Name    string   `json:"name"`
	Targets []string `json:"targets"`

	// Exactly one of Skill, MCP, Config, or Plugin is set (matching Kind).
	Skill  *SkillPayload  `json:"skill,omitempty"`
	MCP    *MCPPayload    `json:"mcp,omitempty"`
	Config *ConfigPayload `json:"config,omitempty"`
	Plugin *PluginPayload `json:"plugin,omitempty"`

	// Payload is the relative path within the staging dir to the artifact payload.
	Payload string `json:"payload,omitempty"`
	// Sensitive indicates this artifact's secret values are in /run/spawnery/secrets.
	Sensitive bool `json:"sensitive,omitempty"`
}

// Manifest is the index file at <staging-dir>/manifest.json.
type Manifest struct {
	// SchemaVersion is the manifest schema major version. Absent/0 is treated
	// as a legacy manifest and accepted; a value greater than CurrentSchemaVersion
	// is rejected by LoadManifest.
	SchemaVersion int        `json:"schema_version"`
	Artifacts     []Artifact `json:"artifacts"`
}

// LoadManifest reads and parses manifest.json from the given staging directory,
// rejecting a manifest whose schema_version exceeds CurrentSchemaVersion.
func LoadManifest(dir string) (Manifest, error) {
	path := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if m.SchemaVersion > CurrentSchemaVersion {
		return Manifest{}, fmt.Errorf("manifest %s has schema_version %d, which is newer than this build understands (max %d); update agentinstall",
			path, m.SchemaVersion, CurrentSchemaVersion)
	}
	return m, nil
}
