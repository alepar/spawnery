package agentinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

// validatePluginIdent validates a plugin or marketplace identifier.
// It must be non-empty and must not contain '/', '\', or '@'.
func validatePluginIdent(field, v string) error {
	if v == "" {
		return fmt.Errorf("plugin %s must not be empty", field)
	}
	if strings.ContainsAny(v, "/\\@") {
		return fmt.Errorf("plugin %s must not contain '/', '\\' or '@': %q", field, v)
	}
	return nil
}

// pluginRef returns the "<plugin>@<marketplace>" enable key.
func pluginRef(p *PluginPayload) string { return p.Plugin + "@" + p.Marketplace }

// ---- claudeEmitter ----------------------------------------------------------

// InstallPlugin writes enabledPlugins and extraKnownMarketplaces to ~/.claude/settings.json.
func (e claudeEmitter) InstallPlugin(a Artifact, _ Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindPlugin, Name: a.Name}
	if a.Plugin == nil {
		base.Status = StatusSkipped
		base.Reason = "plugin artifact has no Plugin payload"
		return base
	}
	if err := validatePluginIdent("plugin", a.Plugin.Plugin); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}
	if err := validatePluginIdent("marketplace", a.Plugin.Marketplace); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	additions := map[string]interface{}{
		"enabledPlugins": map[string]interface{}{pluginRef(a.Plugin): true},
	}
	if a.Plugin.Source != "" {
		additions["extraKnownMarketplaces"] = map[string]interface{}{
			a.Plugin.Marketplace: map[string]interface{}{"source": a.Plugin.Source},
		}
	}

	path := e.layout.ConfigPath
	root, err := readJSONMap(path)
	if err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}
	deepMerge(root, additions)

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("marshal %s: %v", path, err)
		return base
	}
	if err := atomicWriteFile(path, data, filePerm(path, false, 0o644)); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	base.Status = StatusApplied
	base.Capability = CapabilityApplied
	return base
}

// ---- codexEmitter -----------------------------------------------------------

// InstallPlugin writes [plugins."<plugin>@<marketplace>"] and [marketplaces.<marketplace>]
// to $CODEX_HOME/config.toml.  If the plugin's marketplace requires OAuth ON_INSTALL,
// the install is a no-op (cannot complete headless) and Capability=unsupported.
func (e codexEmitter) InstallPlugin(a Artifact, _ Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindPlugin, Name: a.Name}
	if a.Plugin == nil {
		base.Status = StatusSkipped
		base.Reason = "plugin artifact has no Plugin payload"
		return base
	}
	if err := validatePluginIdent("plugin", a.Plugin.Plugin); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}
	if err := validatePluginIdent("marketplace", a.Plugin.Marketplace); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}
	if a.Plugin.RequiresOAuth {
		base.Status = StatusSkipped
		base.Capability = CapabilityUnsupported
		base.Reason = "codex plugin requires OAuth ON_INSTALL; cannot complete headless"
		return base
	}

	additions := map[string]interface{}{
		"plugins": map[string]interface{}{
			pluginRef(a.Plugin): map[string]interface{}{"enabled": true},
		},
	}
	if a.Plugin.Source != "" {
		additions["marketplaces"] = map[string]interface{}{
			a.Plugin.Marketplace: map[string]interface{}{"source": a.Plugin.Source},
		}
	}

	path := e.layout.ConfigPath
	var doc map[string]interface{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			doc = make(map[string]interface{})
		} else {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("read %s: %v", path, err)
			return base
		}
	} else {
		if err := toml.Unmarshal(data, &doc); err != nil {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("parse %s: %v", path, err)
			return base
		}
		if doc == nil {
			doc = make(map[string]interface{})
		}
	}

	deepMerge(doc, additions)

	out, err := toml.Marshal(doc)
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("marshal %s: %v", path, err)
		return base
	}
	if err := atomicWriteFile(path, out, filePerm(path, false, 0o644)); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	base.Status = StatusApplied
	base.Capability = CapabilityApplied
	return base
}

// ---- opencodeEmitter --------------------------------------------------------

// InstallPlugin registers a plugin entry in the ~/.config/opencode/opencode.json
// plugin:[] array.  LocalFile is preferred (applied, offline); NPM is best-effort
// (configured but needs registry.npmjs.org egress at cold start).
func (e opencodeEmitter) InstallPlugin(a Artifact, _ Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindPlugin, Name: a.Name}
	if a.Plugin == nil {
		base.Status = StatusSkipped
		base.Reason = "plugin artifact has no Plugin payload"
		return base
	}

	entry := a.Plugin.LocalFile
	cap := CapabilityApplied
	reason := ""
	if entry == "" && a.Plugin.NPM != "" {
		entry = a.Plugin.NPM
		cap = CapabilityBestEffort
		reason = "opencode npm plugin requires registry.npmjs.org egress at cold start; configured != active"
	}
	if entry == "" {
		base.Status = StatusSkipped
		base.Capability = CapabilityUnsupported
		base.Reason = "opencode plugin requires localFile or npm source"
		return base
	}

	path := e.layout.ConfigPath

	var raw []byte
	if data, err := os.ReadFile(path); err != nil {
		if os.IsNotExist(err) {
			raw = []byte("{}")
		} else {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("read %s: %v", path, err)
			return base
		}
	} else {
		raw = data
	}

	v, err := hujson.Parse(raw)
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("parse JSONC %s: %v", path, err)
		return base
	}

	stdCopy, err := hujson.Standardize(v.Pack())
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("standardize %s: %v", path, err)
		return base
	}

	var existing map[string]interface{}
	if err := json.Unmarshal(stdCopy, &existing); err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("unmarshal %s: %v", path, err)
		return base
	}

	// Build deduped plugin array.
	var arr []interface{}
	if cur, ok := existing["plugin"].([]interface{}); ok {
		arr = cur
	}
	for _, it := range arr {
		if s, ok := it.(string); ok && s == entry {
			// Already present — idempotent applied.
			base.Status = StatusApplied
			base.Capability = cap
			base.Reason = reason
			return base
		}
	}
	arr = append(arr, entry)

	patchVal, err := json.Marshal(arr)
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("marshal plugin array: %v", err)
		return base
	}

	patch := fmt.Sprintf(`[{"op":"add","path":"/%s","value":%s}]`, jsonPointerEscape("plugin"), patchVal)
	if err := v.Patch([]byte(patch)); err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("patch /plugin: %v", err)
		return base
	}

	if err := atomicWriteFile(path, v.Pack(), filePerm(path, false, 0o644)); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	base.Status = StatusApplied
	base.Capability = cap
	base.Reason = reason
	return base
}
