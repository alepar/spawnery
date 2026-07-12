package agentinstall

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// hermesEmitter handles artifact installation for the hermes agent.
// MCP and config are deferred to sp-mofj. Skill install is implemented: canonical-only,
// same as opencode/goose.
type hermesEmitter struct {
	baseEmitter
}

func newHermesEmitter(homeDir string) hermesEmitter {
	configRoot := filepath.Join(homeDir, ".hermes")
	return hermesEmitter{
		baseEmitter: baseEmitter{
			layout: AgentLayout{
				Name:       "hermes",
				ConfigRoot: configRoot,
				// SkillPath is blank: ~/.agents/skills is the canonical dir, implicit for
				// every agent (CanonicalSkillsDir), not a hermes-specific native copy target.
				// hermes additionally needs a `skills.external_dirs` upsert in config.yaml to
				// read it — sp-mwco.2.5's glue, spike-confirmed required and sufficient.
				SkillPath:     "",
				MCPPath:       filepath.Join(configRoot, "config.yaml"),
				MCPFormat:     FormatYAML,
				ConfigPath:    filepath.Join(configRoot, "config.yaml"),
				ConfigFormat:  FormatYAML,
				SchemaVersion: "hermes-1.0",
			},
		},
	}
}

const hermesReason = "deferred to sp-mofj"

// InstallSkill installs a skill into the canonical ~/.agents/skills/<name>/ dir, then
// upserts skills.external_dirs in ~/.hermes/config.yaml so hermes actually reads it
// (spike-confirmed required and sufficient — sp-mwco.2.2/2.5). If the canonical install
// didn't apply, the glue step is skipped and its report returned unchanged. If the glue
// write fails, the report is downgraded to StatusFailed: files installed but unreadable
// by hermes is a failed install from the user's perspective — fail closed, deliberately,
// so sp-mwco.2.7's apply-report fails the spawn rather than reporting a false success.
func (e hermesEmitter) InstallSkill(a Artifact, opts Options) Report {
	report := installSkill(e.layout, a, opts)
	if report.Status != StatusApplied {
		return report
	}

	changed, err := upsertHermesExternalDirs(e.layout.ConfigPath, CanonicalSkillsDir(opts.HomeDir))
	if err != nil {
		report.Status = StatusFailed
		report.Reason = fmt.Sprintf("skill files installed but hermes config.yaml glue failed: %v", err)
		return report
	}
	if changed {
		glueMsg := fmt.Sprintf("upserted skills.external_dirs in %s", e.layout.ConfigPath)
		if report.Reason != "" {
			report.Reason += "; " + glueMsg
		} else {
			report.Reason = glueMsg
		}
	}
	return report
}

// upsertHermesExternalDirs ensures dir is present in the skills.external_dirs sequence
// of the YAML config at path, creating the file (and its 0700 parent dir) if absent.
// It merges into whatever is already there — path may be launcher-written (a model:
// block with an api_key) — and never clobbers unrelated keys or coerces a wrong-typed
// skills/external_dirs value away; either is reported as an error instead so the
// pre-existing file is left untouched.
//
// changed is false (no write performed) when dir was already present.
//
// Documented cost, accepted: the map[string]any round-trip (yaml.Unmarshal ->
// yaml.Marshal) drops YAML comments and reorders keys alphabetically. The file is
// container-generated (deploy/agent/launch's `cat > … <<EOF`), so this is accepted —
// key/value content surviving the round-trip is what matters, not formatting.
func upsertHermesExternalDirs(path, dir string) (changed bool, err error) {
	data, readErr := readFileOrEmpty(path)
	if readErr != nil {
		return false, readErr
	}

	var root map[string]interface{}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if root == nil {
		root = make(map[string]interface{})
	}

	skills, err := upsertYAMLStringMap(root, "skills", path)
	if err != nil {
		return false, err
	}

	var dirs []interface{}
	if raw, ok := skills["external_dirs"]; ok {
		seq, ok := raw.([]interface{})
		if !ok {
			return false, fmt.Errorf("%s: skills.external_dirs is not a list (got %T)", path, raw)
		}
		dirs = seq
	}
	for _, d := range dirs {
		if s, ok := d.(string); ok && s == dir {
			return false, nil
		}
	}
	skills["external_dirs"] = append(dirs, dir)

	out, err := yaml.Marshal(root)
	if err != nil {
		return false, fmt.Errorf("marshal %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := atomicWriteFile(path, out, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// readFileOrEmpty reads path, returning (nil, nil) if it does not exist.
func readFileOrEmpty(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return data, nil
	}
	if os.IsNotExist(err) {
		return nil, nil
	}
	return nil, fmt.Errorf("read %s: %w", path, err)
}

// upsertYAMLStringMap ensures root[key] is a map[string]interface{}, creating it if
// absent. Returns an error (never coerces) if root[key] exists with a different type.
func upsertYAMLStringMap(root map[string]interface{}, key, path string) (map[string]interface{}, error) {
	raw, ok := root[key]
	if !ok {
		m := make(map[string]interface{})
		root[key] = m
		return m, nil
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s: %s is not a mapping (got %T)", path, key, raw)
	}
	return m, nil
}

// InstallMCP is deferred to sp-mofj.
func (e hermesEmitter) InstallMCP(a Artifact, _ Options) Report {
	return Report{
		Agent:  e.layout.Name,
		Kind:   KindMCP,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: hermesReason,
	}
}

// ApplyConfig is deferred to sp-mofj.
func (e hermesEmitter) ApplyConfig(a Artifact, _ Options) Report {
	return Report{
		Agent:  e.layout.Name,
		Kind:   KindConfig,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: hermesReason,
	}
}

// Capabilities returns the support matrix for hermes: skill=supported (this emitter
// writes the external_dirs glue that makes the canonical dir readable), everything
// else no-op (MCP/config deferred to sp-mofj).
func (e hermesEmitter) Capabilities() map[Kind]CapabilityStatus {
	return map[Kind]CapabilityStatus{
		KindSkill:            CapStatusSupported,
		KindMCP:              CapStatusNoOp,
		KindConfig:           CapStatusNoOp,
		KindPlugin:           CapStatusNoOp,
		Kind("instructions"): CapStatusNoOp,
	}
}
