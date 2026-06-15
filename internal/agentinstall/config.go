package agentinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

// Normalized config key names (canonical grammar).
const (
	// ckApprovalPosture is the canonical key for agent approval / permission posture.
	// Valid values: "always-ask" | "ask-risky" | "auto" | "yolo" (default).
	ckApprovalPosture = "approvalPosture"
	// ckDisabledTools lists tool names to disable in the agent.
	ckDisabledTools = "disabledTools"
	// ckAllowedCommands lists shell command / glob patterns to allow.
	ckAllowedCommands = "allowedCommands"
	// ckDeniedCommands lists shell command / glob patterns to deny.
	ckDeniedCommands = "deniedCommands"
)

// approvalPostureClaudeMap maps canonical approvalPosture values to
// claude permissions.defaultMode values.  "auto" is best-effort: claude has no
// model-tier-gated autonomous mode so it is mapped to the nearest alternative.
var approvalPostureClaudeMap = map[string]string{
	"always-ask": "default",
	"ask-risky":  "acceptEdits",
	"auto":       "acceptEdits", // best-effort
	"yolo":       "bypassPermissions",
}

// approvalPostureCodexMap maps canonical approvalPosture values to
// codex approval_policy values.  "auto" is best-effort: model-tier gating is not
// expressible in codex config.
var approvalPostureCodexMap = map[string]string{
	"always-ask": "untrusted",
	"ask-risky":  "on-request",
	"auto":       "on-failure", // best-effort
	"yolo":       "never",
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

// toStringSlice converts an interface{} value to a []string, handling both
// []interface{} (from JSON deserialization) and []string inputs.
func toStringSlice(v interface{}) []string {
	switch tv := v.(type) {
	case []string:
		return tv
	case []interface{}:
		out := make([]string, 0, len(tv))
		for _, elem := range tv {
			if s, ok := elem.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{tv}
	}
	return nil
}

// getOrCreateNestedMap ensures native[keys[0]][keys[1]]... exists as a
// map[string]interface{} and returns the innermost map.
func getOrCreateNestedMap(native map[string]interface{}, keys ...string) map[string]interface{} {
	current := native
	for _, key := range keys {
		if next, ok := current[key]; ok {
			if nextMap, ok := next.(map[string]interface{}); ok {
				current = nextMap
				continue
			}
		}
		m := make(map[string]interface{})
		current[key] = m
		current = m
	}
	return current
}

// translateNormalized converts normalized config keys to agent-native key/value
// additions and an optional codex command policy.
//
// forbidden is a set built from the agent's ForbiddenConfigKeys: any normalized
// key whose NAME appears in this set is rejected (never emitted) regardless of
// translation — this prevents callers from injecting native sandbox/permission
// keys disguised as normalized ones.
//
// Returns:
//
//	native    – map to deep-merge into the agent's config file
//	cap       – aggregate capability of all keys processed
//	reasons   – per-key skip / best-effort / rejection notes
//	codexCmd  – non-nil when codex command policy must be written to rules file
func translateNormalized(agent string, norm map[string]interface{}, forbidden map[string]bool) (native map[string]interface{}, cap Capability, reasons []string, codexCmd *commandPolicy) {
	native = make(map[string]interface{})
	var hasBestEffort, hasApplied bool

	// For claude: accumulate permissions sub-map to avoid multiple native["permissions"]
	// assignments that would clobber each other.
	var claudePerms map[string]interface{}
	getClaudePerms := func() map[string]interface{} {
		if claudePerms == nil {
			claudePerms = make(map[string]interface{})
		}
		return claudePerms
	}

	// For codex: accumulate command policy (written to rules file, not config.toml).
	var cp commandPolicy

	for k, v := range norm {
		// Hardening: reject any normalized key whose name is in the forbidden set.
		// This prevents native sandbox / permission keys from being injected via
		// the normalized pathway.
		if forbidden[k] {
			reasons = append(reasons, fmt.Sprintf("forbidden normalized key %q rejected", k))
			continue // contributes to unsupported (no flag set)
		}

		switch k {
		case ckApprovalPosture:
			val, _ := v.(string)
			switch agent {
			case "claude":
				if mapped, ok := approvalPostureClaudeMap[val]; ok {
					getClaudePerms()["defaultMode"] = mapped
					if val == "auto" {
						hasBestEffort = true
						reasons = append(reasons, "claude has no model-tier-gated autonomous mode; mapped to acceptEdits")
					} else {
						hasApplied = true
					}
				} else {
					reasons = append(reasons, fmt.Sprintf("approvalPosture value %q not recognized", val))
				}
			case "codex":
				if mapped, ok := approvalPostureCodexMap[val]; ok {
					native["approval_policy"] = mapped
					if val == "auto" {
						hasBestEffort = true
						reasons = append(reasons, "codex 'auto' mapped to limited on-failure approvals; model-tier gating not expressible")
					} else {
						hasApplied = true
					}
				} else {
					reasons = append(reasons, fmt.Sprintf("approvalPosture value %q not recognized", val))
				}
			case "opencode":
				reasons = append(reasons, "approvalPosture not mapped for opencode; use native passthrough")
			default:
				reasons = append(reasons, fmt.Sprintf("approvalPosture not mapped for %s", agent))
			}

		case ckDisabledTools:
			tools := toStringSlice(v)
			if len(tools) == 0 {
				break
			}
			switch agent {
			case "opencode":
				toolsMap := make(map[string]interface{}, len(tools))
				for _, t := range tools {
					toolsMap[t] = false
				}
				native["tools"] = toolsMap
				hasApplied = true
			case "claude":
				perms := getClaudePerms()
				existing, _ := perms["deny"].([]interface{})
				for _, t := range tools {
					existing = append(existing, t)
				}
				perms["deny"] = existing
				hasApplied = true
			default:
				// codex and any other agent have no per-tool disable.
				reasons = append(reasons, fmt.Sprintf("%s has no per-tool disable", agent))
			}

		case ckAllowedCommands:
			pats := toStringSlice(v)
			if len(pats) == 0 {
				break
			}
			switch agent {
			case "claude":
				perms := getClaudePerms()
				existing, _ := perms["allow"].([]interface{})
				for _, p := range pats {
					existing = append(existing, fmt.Sprintf("Bash(%s)", p))
				}
				perms["allow"] = existing
				hasApplied = true
			case "opencode":
				bashMap := getOrCreateNestedMap(native, "permission", "bash")
				for _, p := range pats {
					bashMap[p] = "allow"
				}
				hasApplied = true
			case "codex":
				cp.Allowed = append(cp.Allowed, pats...)
				hasApplied = true
			default:
				reasons = append(reasons, fmt.Sprintf("allowedCommands not mapped for %s", agent))
			}

		case ckDeniedCommands:
			pats := toStringSlice(v)
			if len(pats) == 0 {
				break
			}
			switch agent {
			case "claude":
				perms := getClaudePerms()
				existing, _ := perms["deny"].([]interface{})
				for _, p := range pats {
					existing = append(existing, fmt.Sprintf("Bash(%s)", p))
				}
				perms["deny"] = existing
				hasApplied = true
			case "opencode":
				bashMap := getOrCreateNestedMap(native, "permission", "bash")
				for _, p := range pats {
					bashMap[p] = "deny"
				}
				hasApplied = true
			case "codex":
				cp.Denied = append(cp.Denied, pats...)
				hasApplied = true
			default:
				reasons = append(reasons, fmt.Sprintf("deniedCommands not mapped for %s", agent))
			}

		default:
			reasons = append(reasons, fmt.Sprintf("unknown normalized key %q", k))
		}
	}

	// Commit accumulated claude permissions to the native map.
	if claudePerms != nil {
		native["permissions"] = claudePerms
	}

	// Commit codex command policy (non-empty only).
	if !cp.empty() {
		codexCmd = &cp
	}

	// Aggregate capability across all keys processed.
	if hasBestEffort {
		cap = CapabilityBestEffort
	} else if hasApplied {
		cap = CapabilityApplied
	} else {
		cap = CapabilityUnsupported
	}

	return native, cap, reasons, codexCmd
}

// buildAdditions constructs the final additions map for a given agent from the ConfigPayload.
//
// Order: native passthrough (filtered) is applied first; normalized-derived keys win on conflict.
// Returns the additions map, aggregate capability, all reason strings, and any codex command policy.
func buildAdditions(agentName string, forbiddenSlice []string, cfg *ConfigPayload) (additions map[string]interface{}, cap Capability, reasons []string, codexCmd *commandPolicy) {
	additions = make(map[string]interface{})

	// Build the forbidden set once for O(1) lookups.
	forbidden := make(map[string]bool, len(forbiddenSlice))
	for _, k := range forbiddenSlice {
		forbidden[k] = true
	}

	// 1. Native passthrough: filter forbidden keys then merge.
	nativeContributed := false
	if v, ok := cfg.Native[agentName]; ok {
		if nativeMap, ok := v.(map[string]interface{}); ok {
			kept, dropped := filterForbidden(nativeMap, forbiddenSlice)
			for _, k := range dropped {
				reasons = append(reasons, fmt.Sprintf("forbidden native key %q dropped", k))
			}
			if len(kept) > 0 {
				deepMerge(additions, kept)
				nativeContributed = true
			}
		}
	}

	// 2. Normalized keys translated to native (wins over passthrough on conflict).
	normalizedNative, normCap, normReasons, cp := translateNormalized(agentName, cfg.Normalized, forbidden)
	reasons = append(reasons, normReasons...)
	deepMerge(additions, normalizedNative)
	codexCmd = cp

	// 3. Aggregate capability: normCap takes precedence; native passthrough upgrades
	// unsupported to applied (something was written).
	if normCap == CapabilityBestEffort {
		cap = CapabilityBestEffort
	} else if normCap == CapabilityApplied || nativeContributed {
		cap = CapabilityApplied
	} else {
		cap = CapabilityUnsupported
	}

	return additions, cap, reasons, codexCmd
}

// skipReport returns a Skipped report with the given base, reason, capability, and skip notes.
func skipReport(base Report, reason string, cap Capability, skips []string) Report {
	if len(skips) > 0 {
		reason = reason + ": " + strings.Join(skips, "; ")
	}
	base.Status = StatusSkipped
	base.Capability = cap
	base.Reason = reason
	return base
}

// appliedReport returns an Applied report with Capability stamped and any reason notes joined.
func appliedReport(base Report, cap Capability, skips []string) Report {
	base.Status = StatusApplied
	base.Capability = cap
	if len(skips) > 0 {
		base.Reason = strings.Join(skips, "; ")
	}
	return base
}

// ---- claudeEmitter ----------------------------------------------------------

// ApplyConfig applies config keys to ~/.claude/settings.json (JSON format, USER scope).
// Normalized keys are translated per-agent; native passthrough is deep-merged.
// Normalized-derived keys win over native passthrough on conflict.
// Forbidden keys ("model", "permissions") in native passthrough are dropped and reported.
// "permissions" is managed via normalized keys (approvalPosture, disabledTools,
// allowedCommands, deniedCommands) and must not be overridden wholesale via passthrough.
func (e claudeEmitter) ApplyConfig(a Artifact, opts Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindConfig, Name: a.Name}

	if a.Config == nil {
		base.Status = StatusSkipped
		base.Reason = "config artifact has no Config payload"
		return base
	}

	additions, cap, allSkips, _ := buildAdditions(e.layout.Name, e.layout.ForbiddenConfigKeys, a.Config)

	if len(additions) == 0 {
		return skipReport(base, "no applicable config additions", cap, allSkips)
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

	return appliedReport(base, cap, allSkips)
}

// ---- codexEmitter -----------------------------------------------------------

// ApplyConfig applies config keys to $CODEX_HOME/config.toml (TOML format, USER scope).
// The same file is shared with MCP server entries; all existing keys survive via deep-merge.
// Forbidden keys ("model", "approval_policy", "sandbox_mode", "sandbox_workspace_write")
// in native passthrough are dropped and reported.
// allowedCommands / deniedCommands are written to $CODEX_HOME/rules/default.rules
// (via the execpolicy adapter) rather than config.toml.
func (e codexEmitter) ApplyConfig(a Artifact, opts Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindConfig, Name: a.Name}

	if a.Config == nil {
		base.Status = StatusSkipped
		base.Reason = "config artifact has no Config payload"
		return base
	}

	additions, cap, allSkips, codexCmd := buildAdditions(e.layout.Name, e.layout.ForbiddenConfigKeys, a.Config)

	if len(additions) == 0 && (codexCmd == nil || codexCmd.empty()) {
		return skipReport(base, "no applicable config additions", cap, allSkips)
	}

	// Write config.toml if there are native/normalized config additions.
	if len(additions) > 0 {
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

		perm := filePerm(path, false, 0o644)
		if err := atomicWriteFile(path, out, perm); err != nil {
			base.Status = StatusFailed
			base.Reason = err.Error()
			return base
		}
	}

	// Write execpolicy rules file if allowedCommands / deniedCommands were given.
	if codexCmd != nil && !codexCmd.empty() {
		if _, err := writeCodexExecPolicy(e.layout.RulesDir, *codexCmd); err != nil {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("write execpolicy: %v", err)
			return base
		}
	}

	return appliedReport(base, cap, allSkips)
}

// ---- opencodeEmitter --------------------------------------------------------

// ApplyConfig applies config keys to ~/.config/opencode/opencode.json (JSONC, USER scope).
// Uses format-preserving hujson patching so existing comments on untouched sibling keys survive.
// For each top-level key in the additions, existing nested maps are deep-merged before patching.
// Forbidden keys ("model", "permission") in native passthrough are dropped and reported.
// approvalPosture is not mapped for opencode (hard-errors on invalid config).
// allowedCommands/deniedCommands are written via the normalized permission.bash map.
func (e opencodeEmitter) ApplyConfig(a Artifact, opts Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindConfig, Name: a.Name}

	if a.Config == nil {
		base.Status = StatusSkipped
		base.Reason = "config artifact has no Config payload"
		return base
	}

	additions, cap, allSkips, _ := buildAdditions(e.layout.Name, e.layout.ForbiddenConfigKeys, a.Config)

	if len(additions) == 0 {
		return skipReport(base, "no applicable config additions", cap, allSkips)
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

	return appliedReport(base, cap, allSkips)
}
