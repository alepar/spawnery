//go:build e2e

package cp_test

// skill_paths_e2e_test.go is the SHARED canonical-path helper for the sp-mwco skills-install e2e
// suite (sp-mwco.2.9 + sp-mwco.1.10 both call into this file from the same cp_test package).
// Deliberately test-function-free to keep the merge surface at zero: it only exports helpers.
//
// Placement truth is DERIVED from the production agentinstall registry + agentcaps runnable/binary
// vocabulary — never hardcoded — so a future emitter change breaks this test loudly instead of
// silently drifting. Mirrors the derivation already used at
// internal/agentinstall/conformance_test.go:39-43 (skillDestDirs).

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"spawnery/internal/agentcaps"
	"spawnery/internal/agentinstall"
)

// skillsHomeEnv is the HOME the agent containers run under (root — see deploy/agent's launch
// scripts / apply-artifacts.sh, which install under /root).
const skillsHomeEnv = "/root"

// expectedSkillDirs returns the absolute in-container directories a skill named `name` must exist
// in for `agent` (an agentinstall.Registry key, e.g. "claude", "goose" — NOT a runnable ID; use
// agentForRunnable to convert). Always includes the canonical dir
// (agentinstall.CanonicalSkillsDir); additionally includes layout.SkillPath when the agent's
// emitter declares a real native copy (claude, codex today).
func expectedSkillDirs(agent, name string) []string {
	reg := agentinstall.NewRegistry(agentinstall.MapEnviron{"HOME": skillsHomeEnv})
	layout, ok := reg.Lookup(agent)
	if !ok {
		return nil
	}
	l := layout.Layout()
	dirs := []string{agentinstall.CanonicalSkillsDir(skillsHomeEnv)}
	if l.SkillPath != "" {
		dirs = append(dirs, l.SkillPath)
	}
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d + "/" + name
	}
	return out
}

// agentForRunnable maps a runnable ID (e.g. "goose-acp") to its agentinstall emitter name (e.g.
// "goose"), delegating to agentcaps.EmitterForRunnable so the runnable<->emitter mapping stays
// single-sourced with production (internal/agentcaps/emitter.go). Fails the test if the runnable
// is unknown or has no emitter — every runnable this suite drives is expected to have one.
func agentForRunnable(t *testing.T, runnableID string) string {
	t.Helper()
	emitter, ok := agentcaps.EmitterForRunnable(runnableID)
	if !ok {
		t.Fatalf("agentForRunnable: no emitter for runnable %q (agentcaps/emitter.go binaryEmitter table)", runnableID)
	}
	return emitter
}

// assertSkillPlaced docker-execs `test -f <dir>/SKILL.md` for every dir expectedSkillDirs returns
// for (agent, name) against container. On a miss it fails with a `find` dump of the canonical +
// claude + codex skill roots to aid diagnosis.
func assertSkillPlaced(ctx context.Context, t *testing.T, container, agent, name string) {
	t.Helper()
	dirs := expectedSkillDirs(agent, name)
	if len(dirs) == 0 {
		t.Fatalf("assertSkillPlaced: agent %q has no registered emitter (nothing to check)", agent)
	}
	for _, dir := range dirs {
		path := dir + "/SKILL.md"
		if err := dockerTestFile(ctx, container, path); err != nil {
			t.Fatalf("skill %q not placed at %s for agent %q (container %s): %v\n%s",
				name, path, agent, container, err, dumpSkillTrees(ctx, container))
		}
	}
}

// assertSkillAbsent is the Targets-negative counterpart: asserts NONE of expectedSkillDirs' paths
// carry a SKILL.md for (agent, name) in container — used to prove a skill scoped to a different
// agent's Targets did not leak into this pod.
func assertSkillAbsent(ctx context.Context, t *testing.T, container, agent, name string) {
	t.Helper()
	dirs := expectedSkillDirs(agent, name)
	for _, dir := range dirs {
		path := dir + "/SKILL.md"
		if err := dockerTestFile(ctx, container, path); err == nil {
			t.Fatalf("skill %q unexpectedly present at %s for agent %q (container %s) — Targets scoping leaked",
				name, path, agent, container)
		}
	}
}

// dockerTestFile runs `docker exec <container> test -f <path>`, returning nil iff the file exists.
func dockerTestFile(ctx context.Context, container, path string) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "test", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dumpSkillTrees returns a `find` dump of the canonical + claude + codex skill roots inside
// container, for diagnostics on an assertSkillPlaced failure. Best-effort: errors are folded into
// the returned text rather than propagated.
func dumpSkillTrees(ctx context.Context, container string) string {
	out, _ := exec.CommandContext(ctx, "docker", "exec", container,
		"find", "/root/.agents/skills", "/root/.claude/skills", "/root/.codex/skills",
		"-name", "SKILL.md", "-o", "-type", "d").CombinedOutput()
	return "skill trees:\n" + string(out)
}
