package agentinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

// ckApprovalPosture is the normalized key for the agent approval/permission posture.
const ckApprovalPosture = "approvalPosture"

// approvalPostureCodex maps normalized approvalPosture values to codex approval_policy values (1:1).
// The native key name is "approval_policy"; the values are identical to the normalized ones.
var approvalPostureCodex = map[string]bool{
	"untrusted":  true,
	"on-failure": true,
	"on-request": true,
	"never":      true,
}

// approvalPostureClaude maps normalized approvalPosture values to claude permissions.defaultMode values.
// "on-request" and "on-failure" are not cleanly mappable and are skipped.
var approvalPostureClaude = map[string]string{
	"never":     "bypassPermissions",
	"untrusted": "default",
}

// deepMerge merges src into dst recursively.
// Nested maps are merged; scalars and slices in src overwrite those in dst.
func deepMerge(dst, src map[string]interface{}) {
	for k, sv := range src {
		if dv, ok := dst[k]; ok {
			if dm, ok := dv.(map[string]interface{}); ok {
				if sm, ok := sv.(map[string]interface{}); ok {
					deepMerge(dm, sm)
					continue
				}
			}
		}
		dst[k] = sv
	}
}

// filterForbidden returns a copy of native with top-level forbidden keys removed.
// dropped lists the removed key names.
func filterForbidden(native map[string]interface{}, forbidden []string) (map[string]interface{}, []string) {
	if len(native) == 0 {
		return native, nil
	}
	forbidSet := make(map[string]bool, len(forbidden))
	for _, k := range forbidden {
		forbidSet[k] = true
	}
	kept := make(map[string]interface{}, len(native))
	var dropped []string
	for k, v := range native {
		if forbidSet[k] {
			dropped = append(dropped, k)
		} else {
			kept[k] = v
		}
	}
	return kept, dropped
}

// translateNormalized converts normalized config keys to agent-native key/value additions.
// Unknown normalized keys are skipped with a descriptive reason string.
// Returns the native additions map and a slice of skip reason strings.
func translateNormalized(agent string, norm map[string]interface{}) (map[string]interface{}, []string) {
	native := make(map[string]interface{})
	var skips []string

	for k, v := range norm {
		switch k {
		case ckApprovalPosture:
			val, _ := v.(string)
			switch agent {
			case "codex":
				if approvalPostureCodex[val] {
					// Native key is approval_policy; values are identical to normalized.
					native["approval_policy"] = val
				} else {
					skips = append(skips, fmt.Sprintf("approvalPosture value %q not recognized for codex", val))
				}
			case "claude":
				if mapped, ok := approvalPostureClaude[val]; ok {
					// claude uses permissions.defaultMode (nested).
					perms := make(map[string]interface{})
					perms["defaultMode"] = mapped
					native["permissions"] = perms
				} else {
					skips = append(skips, fmt.Sprintf("approvalPosture value %q not mappable for claude", val))
				}
			case "opencode":
				skips = append(skips, "approvalPosture not mapped for opencode; use native passthrough")
			default:
				skips = append(skips, fmt.Sprintf("approvalPosture not mapped for %s", agent))
			}
		default:
			skips = append(skips, fmt.Sprintf("unknown normalized key %q", k))
		}
	}

	return native, skips
}

// buildAdditions constructs the final additions map for a given agent from the ConfigPayload.
// Order: native passthrough (filtered) is applied first, normalized-derived wins on conflict.
// Also returns skip reason strings and forbidden-key drop messages for the Report.
func buildAdditions(agentName string, forbidden []string, cfg *ConfigPayload) (additions map[string]interface{}, allSkips []string) {
	additions = make(map[string]interface{})

	// 1. Native passthrough: filter forbidden keys then merge.
	if v, ok := cfg.Native[agentName]; ok {
		if nativeMap, ok := v.(map[string]interface{}); ok {
			kept, dropped := filterForbidden(nativeMap, forbidden)
			for _, k := range dropped {
				allSkips = append(allSkips, fmt.Sprintf("forbidden native key %q dropped", k))
			}
			deepMerge(additions, kept)
		}
	}

	// 2. Normalized keys translated to native (wins over passthrough).
	normalizedNative, skips := translateNormalized(agentName, cfg.Normalized)
	allSkips = append(allSkips, skips...)
	deepMerge(additions, normalizedNative) // normalized overwrites passthrough on conflict

	return additions, allSkips
}

// skipReport returns a Skipped report with the given base and reason, incorporating any skips.
func skipReport(base Report, reason string, skips []string) Report {
	if len(skips) > 0 {
		reason = reason + ": " + strings.Join(skips, "; ")
	}
	base.Status = StatusSkipped
	base.Reason = reason
	return base
}

// appliedReport returns an Applied report, with Reason set to any skip messages.
func appliedReport(base Report, skips []string) Report {
	base.Status = StatusApplied
	if len(skips) > 0 {
		base.Reason = "skipped keys: " + strings.Join(skips, "; ")
	}
	return base
}

// ---- claudeEmitter ----------------------------------------------------------

// ApplyConfig applies config keys to ~/.claude/settings.json (JSON format, USER scope).
// Normalized keys are translated per-agent; native passthrough is deep-merged.
// Normalized-derived keys win over native passthrough on conflict.
// Forbidden keys (e.g. "model") are dropped and reported.
func (e claudeEmitter) ApplyConfig(a Artifact, opts Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindConfig, Name: a.Name}

	if a.Config == nil {
		base.Status = StatusSkipped
		base.Reason = "config artifact has no Config payload"
		return base
	}

	additions, allSkips := buildAdditions(e.layout.Name, e.layout.ForbiddenConfigKeys, a.Config)

	if len(additions) == 0 {
		return skipReport(base, "no applicable config additions", allSkips)
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

	perm := filePerm(path, false, 0o644)
	if err := atomicWriteFile(path, data, perm); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	return appliedReport(base, allSkips)
}

// ---- codexEmitter -----------------------------------------------------------

// ApplyConfig applies config keys to $CODEX_HOME/config.toml (TOML format, USER scope).
// The same file is shared with MCP server entries; all existing keys survive via deep-merge.
// Forbidden keys (e.g. "model") in native passthrough are dropped and reported.
func (e codexEmitter) ApplyConfig(a Artifact, opts Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindConfig, Name: a.Name}

	if a.Config == nil {
		base.Status = StatusSkipped
		base.Reason = "config artifact has no Config payload"
		return base
	}

	additions, allSkips := buildAdditions(e.layout.Name, e.layout.ForbiddenConfigKeys, a.Config)

	if len(additions) == 0 {
		return skipReport(base, "no applicable config additions", allSkips)
	}

	path := e.layout.ConfigPath

	// Read existing TOML or start fresh.
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

	perm := filePerm(path, false, 0o644)
	if err := atomicWriteFile(path, out, perm); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	return appliedReport(base, allSkips)
}

// ---- opencodeEmitter --------------------------------------------------------

// ApplyConfig applies config keys to ~/.config/opencode/opencode.json (JSONC, USER scope).
// Uses format-preserving hujson patching so existing comments on untouched sibling keys survive.
// For each top-level key in the additions, existing nested maps are deep-merged before patching.
// Forbidden keys in native passthrough are dropped and reported.
// approvalPosture is not mapped for opencode (hard-errors on invalid config); use native passthrough.
func (e opencodeEmitter) ApplyConfig(a Artifact, opts Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindConfig, Name: a.Name}

	if a.Config == nil {
		base.Status = StatusSkipped
		base.Reason = "config artifact has no Config payload"
		return base
	}

	additions, allSkips := buildAdditions(e.layout.Name, e.layout.ForbiddenConfigKeys, a.Config)

	if len(additions) == 0 {
		return skipReport(base, "no applicable config additions", allSkips)
	}

	path := e.layout.ConfigPath

	// Read existing file or start from empty object.
	var raw []byte
	data, err := os.ReadFile(path)
	if err != nil {
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

	// Parse with hujson (JSONC-aware).
	v, err := hujson.Parse(raw)
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("parse JSONC %s: %v", path, err)
		return base
	}

	// Standardize to get a plain JSON copy for reading existing values.
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
	if existing == nil {
		existing = make(map[string]interface{})
	}

	// For each top-level key in additions: deep-merge with existing, then RFC-6902 patch.
	for k, addVal := range additions {
		// If the existing value at this key is also a map, deep-merge to preserve siblings.
		if exV, ok := existing[k]; ok {
			if exMap, ok := exV.(map[string]interface{}); ok {
				if addMap, ok := addVal.(map[string]interface{}); ok {
					merged := make(map[string]interface{})
					deepMerge(merged, exMap)
					deepMerge(merged, addMap)
					addVal = merged
				}
			}
		}

		patchVal, err := json.Marshal(addVal)
		if err != nil {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("marshal patch value for key %q: %v", k, err)
			return base
		}

		ptr := "/" + jsonPointerEscape(k)
		patch := fmt.Sprintf(`[{"op":"add","path":%q,"value":%s}]`, ptr, patchVal)
		if err := v.Patch([]byte(patch)); err != nil {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("patch %s: %v", ptr, err)
			return base
		}
	}

	out := v.Pack()
	perm := filePerm(path, false, 0o644)
	if err := atomicWriteFile(path, out, perm); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	return appliedReport(base, allSkips)
}
