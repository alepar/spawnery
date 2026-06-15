package agentinstall

import (
	"fmt"
	"strings"
)

// Apply processes all artifacts in the manifest against the registry and returns a Result.
//
// Routing rules:
//   - Explicit target names: looked up in the registry; unknown/unsupported → skipped+report.
//   - "all-detected": resolved via Detect(env) intersected with the registry.
//   - Unknown concept (bad kind) → skipped+report.
//   - Per-item skip/failure does NOT fail the run; only catastrophic IO errors are returned.
func Apply(reg Registry, m Manifest, opts Options, env Environ) Result {
	var reports []Report

	for _, artifact := range m.Artifacts {
		targets := resolveTargets(artifact.Targets, reg, env)

		for _, target := range targets {
			var report Report
			switch {
			case target.missing:
				report = Report{
					Agent:  target.name,
					Kind:   artifact.Kind,
					Name:   artifact.Name,
					Status: StatusSkipped,
					Reason: "unknown or unsupported agent",
				}
			default:
				report = dispatchArtifact(target.emitter, artifact, opts)
			}
			reports = append(reports, report)
		}
	}

	return Result{Reports: reports}
}

type resolvedTarget struct {
	name    string
	emitter Emitter
	missing bool
}

// resolveTargets expands the targets field of an artifact to a list of resolvedTargets.
func resolveTargets(targets []string, reg Registry, env Environ) []resolvedTarget {
	// "all-detected" special case: detect present agents and intersect with registry.
	if len(targets) == 1 && targets[0] == "all-detected" {
		detected := Detect(env)
		out := make([]resolvedTarget, 0, len(detected))
		for _, name := range detected {
			if e, ok := reg.Lookup(name); ok {
				out = append(out, resolvedTarget{name: name, emitter: e})
			}
			// Agents detected but not in registry are silently ignored (shouldn't happen with our registry).
		}
		return out
	}

	// Explicit list.
	out := make([]resolvedTarget, 0, len(targets))
	for _, name := range targets {
		if e, ok := reg.Lookup(name); ok {
			out = append(out, resolvedTarget{name: name, emitter: e})
		} else {
			out = append(out, resolvedTarget{name: name, missing: true})
		}
	}
	return out
}

// ApplyFiltered applies only the artifacts that target the named agentFilter, with bounded
// secret-wait for MCP artifacts that carry secretRefs.
//
// Filtering rules:
//   - agentFilter == "": delegates to Apply (no filtering). Caveat: Apply does NO bounded
//     secret-wait, so callers that need secret-wait must provide a non-empty agentFilter.
//   - "all-detected" targets: the artifact always targets the filter agent (unconditionally).
//   - Explicit targets: the artifact is dispatched only if agentFilter is in the list.
//   - Non-targeting artifacts produce no report (silent skip).
//   - Targeting artifacts whose agent is not in the registry produce a skipped report.
//   - KindMCP with secretRefs and opts.SecretWaitTimeout > 0: waits up to the timeout for
//     the secret files; if still missing, produces a StatusSkipped report and skips InstallMCP.
//     Caveat: the wait is per-artifact, so N artifacts each with secretRefs can serialize up
//     to N×SecretWaitTimeout total. TODO(sp-1bia): shared deadline across artifacts.
func ApplyFiltered(reg Registry, m Manifest, opts Options, env Environ, agentFilter string) Result {
	if agentFilter == "" {
		return Apply(reg, m, opts, env)
	}

	var reports []Report

	emitter, emitterOk := reg.Lookup(agentFilter)

	for _, artifact := range m.Artifacts {
		if !artifactTargetsAgent(artifact.Targets, agentFilter) {
			continue // no report — artifact is not meant for this agent
		}

		if !emitterOk {
			reports = append(reports, Report{
				Agent:  agentFilter,
				Kind:   artifact.Kind,
				Name:   artifact.Name,
				Status: StatusSkipped,
				Reason: "unknown or unsupported agent",
			})
			continue
		}

		// For MCP artifacts with secretRefs, perform a bounded wait before dispatch.
		if artifact.Kind == KindMCP && artifact.MCP != nil && len(artifact.MCP.SecretRefs) > 0 && opts.SecretWaitTimeout > 0 {
			missing := waitForSecrets(opts.SecretsDir, artifact.MCP.SecretRefs, opts.SecretWaitTimeout)
			if len(missing) > 0 {
				reports = append(reports, Report{
					Agent:  agentFilter,
					Kind:   artifact.Kind,
					Name:   artifact.Name,
					Status: StatusSkipped,
					Reason: fmt.Sprintf("secret(s) not delivered within %s: %s", opts.SecretWaitTimeout, strings.Join(missing, ", ")),
				})
				continue
			}
		}

		reports = append(reports, dispatchArtifact(emitter, artifact, opts))
	}

	return Result{Reports: reports}
}

// artifactTargetsAgent reports whether the artifact's targets list includes the named agent.
// "all-detected" is treated as targeting every registered agent unconditionally.
func artifactTargetsAgent(targets []string, agent string) bool {
	if len(targets) == 1 && targets[0] == "all-detected" {
		return true
	}
	for _, t := range targets {
		if t == agent {
			return true
		}
	}
	return false
}

// dispatchArtifact calls the appropriate emitter method based on artifact Kind.
func dispatchArtifact(e Emitter, a Artifact, opts Options) Report {
	switch a.Kind {
	case KindSkill:
		return e.InstallSkill(a, opts)
	case KindMCP:
		report := e.InstallMCP(a, opts)
		// Runtime dep check for stdio MCP commands — only when the artifact was
		// actually applied; stamping skipped/deferred reports would be misleading.
		if report.Status == StatusApplied && a.MCP != nil && a.MCP.Stdio != nil && report.RuntimeDepMissing == "" {
			cmd := a.MCP.Stdio.Command
			if cmd != "" && !checkRuntime(cmd) {
				report.RuntimeDepMissing = cmd
			}
		}
		return report
	case KindConfig:
		return e.ApplyConfig(a, opts)
	case KindPlugin:
		return e.InstallPlugin(a, opts)
	default:
		return Report{
			Agent:  e.Layout().Name,
			Kind:   a.Kind,
			Name:   a.Name,
			Status: StatusSkipped,
			Reason: "unknown artifact kind",
		}
	}
}
