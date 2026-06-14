// Package agentinstall is a leaf package (zero spawnery-internal imports).
// It implements the standalone agentinstall CLI adapter seam.
package agentinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Kind is the logical artifact kind.
type Kind string

const (
	KindSkill  Kind = "skill"
	KindMCP    Kind = "mcp"
	KindConfig Kind = "config"
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
}

// Artifact is the canonical descriptor of a single logical artifact.
// Targets is either a list of agent names (claude|codex|opencode|hermes|goose)
// or the string "all-detected".
type Artifact struct {
	Kind    Kind     `json:"kind"`
	Name    string   `json:"name"`
	Targets []string `json:"targets"`

	// Exactly one of Skill, MCP, or Config is set (matching Kind).
	Skill  *SkillPayload  `json:"skill,omitempty"`
	MCP    *MCPPayload    `json:"mcp,omitempty"`
	Config *ConfigPayload `json:"config,omitempty"`

	// Payload is the relative path within the staging dir to the artifact payload.
	Payload string `json:"payload,omitempty"`
	// Sensitive indicates this artifact's secret values are in /run/spawnery/secrets.
	Sensitive bool `json:"sensitive,omitempty"`
}

// Manifest is the index file at <staging-dir>/manifest.json.
type Manifest struct {
	Artifacts []Artifact `json:"artifacts"`
}

// LoadManifest reads and parses a manifest.json from the given staging directory.
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
	return m, nil
}

// Status is the outcome status of a single report entry.
type Status string

const (
	StatusApplied Status = "applied"
	StatusSkipped Status = "skipped"
	StatusFailed  Status = "failed"
)

// Report is the structured outcome for one (artifact × agent) combination.
type Report struct {
	Agent             string `json:"agent"`
	Kind              Kind   `json:"kind"`
	Name              string `json:"name"`
	Status            Status `json:"status"`
	Reason            string `json:"reason,omitempty"`
	RuntimeDepMissing string `json:"runtimeDepMissing,omitempty"`
}

// Result is the JSON-serializable aggregate output of an Apply run.
type Result struct {
	Reports []Report `json:"reports"`
}
