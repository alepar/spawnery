package agentinstall

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

// dispatchArtifact calls the appropriate emitter method based on artifact Kind.
func dispatchArtifact(e Emitter, a Artifact, opts Options) Report {
	switch a.Kind {
	case KindSkill:
		return e.InstallSkill(a, opts)
	case KindMCP:
		report := e.InstallMCP(a, opts)
		// Runtime dep check for stdio MCP commands.
		if a.MCP != nil && a.MCP.Stdio != nil && report.RuntimeDepMissing == "" {
			cmd := a.MCP.Stdio.Command
			if cmd != "" && !checkRuntime(cmd) {
				report.RuntimeDepMissing = cmd
			}
		}
		return report
	case KindConfig:
		return e.ApplyConfig(a, opts)
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
