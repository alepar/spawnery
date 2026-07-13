package main

// Tests for deploy/agent/apply-artifacts.sh via hermetic sh invocations.
// Uses runtime.Caller to locate the helper relative to this test file,
// builds agentinstall onto a temp PATH, and exercises the emitter-map and
// old-image-guard behaviours without a real container.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"spawnery/internal/agentinstall/spec"
	"spawnery/internal/spawnlet"
)

// helperScript returns the absolute path to apply-artifacts.sh relative to this test file.
func helperScript(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile: <repo>/cmd/agentinstall/applyartifacts_test.go
	// helper:   <repo>/deploy/agent/apply-artifacts.sh
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	helper := filepath.Join(repoRoot, "deploy", "agent", "apply-artifacts.sh")
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("apply-artifacts.sh not found at %s: %v", helper, err)
	}
	return helper
}

// buildAgentinstallToDir builds the agentinstall binary into dir and returns its path.
func buildAgentinstallToDir(t *testing.T, dir string) string {
	t.Helper()
	binPath := filepath.Join(dir, "agentinstall")
	cmd := exec.Command("go", "build", "-o", binPath, "spawnery/cmd/agentinstall")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build agentinstall: %v\n%s", err, out)
	}
	return binPath
}

// runHelper runs apply-artifacts.sh with the given runnable and env overrides,
// returning stdout+stderr combined and the exit code.
func runHelper(t *testing.T, helper, runnable string, env []string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", helper, runnable)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run helper: %v", err)
		}
	}
	return string(out), code
}

// TestApplyArtifacts_ClaudeTUIWritesConfig verifies that with a valid manifest targeting
// claude, apply-artifacts.sh for claude-tui writes ~/.claude.json.
func TestApplyArtifacts_ClaudeTUIWritesConfig(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	// Build staging dir with a manifest targeting claude.
	artifactsDir := t.TempDir()
	manifest := `{"artifacts":[{"kind":"mcp","name":"test-mcp","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	secretsDir := t.TempDir()

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + secretsDir,
		"SECRET_WAIT_TIMEOUT=1s",
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\noutput:\n%s", code, out)
	}

	// ~/.claude.json should have been written by agentinstall.
	claudeJSON := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(claudeJSON)
	if err != nil {
		t.Fatalf("~/.claude.json not written (out=%q): %v", out, err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse .claude.json: %v", err)
	}
	servers, ok := root["mcpServers"].(map[string]interface{})
	if !ok || servers["test-mcp"] == nil {
		t.Errorf("test-mcp not found in .claude.json mcpServers: %+v", root)
	}

	// apply-report.json should have been written by the CLI itself (agentinstall --report),
	// under the reserved report/ subdir spawnlet's ArtifactStager makes agent-writable.
	report := filepath.Join(artifactsDir, "report", "apply-report.json")
	data, err = os.ReadFile(report)
	if err != nil {
		t.Fatalf("apply-report.json not written: %v", err)
	}
	if !strings.Contains(string(data), `"outcome":"ok"`) {
		t.Errorf("expected outcome ok in report, got: %s", data)
	}
}

// TestApplyArtifacts_NoOpRunnable verifies that a runnable with no agentcaps/emitter mapping
// (shell — a non-agent runnable, see internal/agentcaps) exits 1 and writes an explicit
// outcome=error report, even with a real agentinstall in PATH and a valid manifest — the CLI
// itself exits 0 with no report when --runnable resolves to no emitter (sp-mwco.2.6); this
// script's post-hoc guard turns that silent success into a legible failure (sp-mwco.2.10) so the
// node does not stall out AwaitApplyReport. The no-op *decision* is still made by
// `agentinstall apply --runnable` in Go, not by a shell `case` here — this only makes its
// silence visible.
//
// NOTE: goose-tui is deliberately NOT used as the no-op example anymore — the shell `case`
// this test used to exercise treated it (wrongly) as a no-op; goose now has a registered
// emitter and genuinely installs (see TestApplyArtifacts_GooseAcpReachesAgentinstall).
func TestApplyArtifacts_NoOpRunnable(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	artifactsDir := t.TempDir()
	manifest := `{"artifacts":[{"kind":"mcp","name":"test-mcp","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
		"SECRET_WAIT_TIMEOUT=1s",
	}

	_, code := runHelper(t, helper, "shell", env)
	if code != 1 {
		t.Fatalf("expected exit 1 for no-op runnable, got %d", code)
	}

	// No config should have been written (shell has no agentinstall emitter).
	claudeJSON := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(claudeJSON); !os.IsNotExist(err) {
		t.Errorf("unexpected write to ~/.claude.json for shell no-op: err=%v", err)
	}

	// The post-hoc guard writes an explicit error report — the CLI itself exits before ever
	// running the apply, so this script must synthesize the report for the node.
	reportPath := filepath.Join(artifactsDir, "report", "apply-report.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("expected apply-report.json to be written for no-op runnable: %v", err)
	}
	rep, err := spec.ParseApplyReport(data)
	if err != nil {
		t.Fatalf("parse apply-report.json: %v\ncontent: %s", err, data)
	}
	if rep.Outcome != spec.OutcomeError {
		t.Errorf("outcome: got %q want %q", rep.Outcome, spec.OutcomeError)
	}
	if rep.Runnable != "shell" {
		t.Errorf("runnable: got %q want shell", rep.Runnable)
	}
	if !strings.Contains(rep.Error, "emitter") || !strings.Contains(rep.Error, "shell") {
		t.Errorf("expected error to mention emitter and shell, got %q", rep.Error)
	}
}

// TestApplyArtifacts_NoEmitterBundle_FailsFastViaReport is the acceptance criterion for
// sp-mwco.2.10: a bundle spawn on a no-emitter runnable must fail immediately with a legible
// reason, not after the node burns out its 2-minute AwaitApplyReport timeout. It runs the real
// script end to end, parses the report it emits, and feeds it straight through the node's own
// verdict function to prove the node would fail fast rather than stall.
func TestApplyArtifacts_NoEmitterBundle_FailsFastViaReport(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	artifactsDir := t.TempDir()
	skillDir := filepath.Join(artifactsDir, "payloads", "s1")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# s1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"artifacts":[{"kind":"skill","name":"s1","targets":["claude"],"bundle":"b1","skill":{"dir":"payloads/s1"}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
		"SECRET_WAIT_TIMEOUT=1s",
	}

	_, code := runHelper(t, helper, "shell", env)
	if code != 1 {
		t.Fatalf("expected exit 1 for no-emitter bundle runnable, got %d", code)
	}

	reportData, err := os.ReadFile(filepath.Join(artifactsDir, "report", "apply-report.json"))
	if err != nil {
		t.Fatalf("apply-report.json not written: %v", err)
	}
	rep, err := spec.ParseApplyReport(reportData)
	if err != nil {
		t.Fatalf("parse apply-report.json: %v\ncontent: %s", err, reportData)
	}

	m, err := spec.LoadManifest(artifactsDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}

	verdictErr, _ := spawnlet.EvaluateApplyReport(m, rep)
	if verdictErr == nil {
		t.Fatal("expected EvaluateApplyReport to return a fatal error for a bundle spawn with an error-outcome report, got nil")
	}
}

// TestApplyArtifacts_GooseAcpReachesAgentinstall verifies goose-acp — a runnable that the OLD
// shell `case` silently no-op'd — now resolves to the goose emitter and actually applies
// (asserted via the report file existing, per the bead's regression fix).
func TestApplyArtifacts_GooseAcpReachesAgentinstall(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	// goose's MCP emitter is deferred (sp-mwco skill work only wired InstallSkill); use a
	// skill artifact, which goose does implement (canonical ~/.agents/skills, no glue).
	artifactsDir := t.TempDir()
	skillDir := filepath.Join(artifactsDir, "payloads", "test-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# test-skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"artifacts":[{"kind":"skill","name":"test-skill","targets":["goose"],"skill":{"dir":"payloads/test-skill"}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
		"SECRET_WAIT_TIMEOUT=1s",
	}

	out, code := runHelper(t, helper, "goose-acp", env)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\noutput:\n%s", code, out)
	}
	report := filepath.Join(artifactsDir, "report", "apply-report.json")
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("apply-report.json not written for goose-acp: %v", err)
	}
	if !strings.Contains(string(data), `"outcome":"ok"`) {
		t.Errorf("expected outcome ok in report, got: %s", data)
	}
}

// TestApplyArtifacts_HermesAcpReachesAgentinstall verifies hermes-acp — another runnable the
// OLD shell `case` silently no-op'd — now resolves to the hermes emitter and actually applies
// (asserted via the report file existing).
func TestApplyArtifacts_HermesAcpReachesAgentinstall(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	// hermes's MCP/config emitters are deferred to sp-mofj; use a skill artifact, which
	// hermes does implement (canonical ~/.agents/skills; the external_dirs glue is
	// sp-mwco.2.5, out of scope here — this test only checks agentinstall was reached).
	artifactsDir := t.TempDir()
	skillDir := filepath.Join(artifactsDir, "payloads", "test-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# test-skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"artifacts":[{"kind":"skill","name":"test-skill","targets":["hermes"],"skill":{"dir":"payloads/test-skill"}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
		"SECRET_WAIT_TIMEOUT=1s",
	}

	out, code := runHelper(t, helper, "hermes-acp", env)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\noutput:\n%s", code, out)
	}
	report := filepath.Join(artifactsDir, "report", "apply-report.json")
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("apply-report.json not written for hermes-acp: %v", err)
	}
	if !strings.Contains(string(data), `"outcome":"ok"`) {
		t.Errorf("expected outcome ok in report, got: %s", data)
	}
}

// TestApplyArtifacts_OldImageGuard verifies that when agentinstall is absent from PATH, the
// helper exits 1, prints a diagnostic to stderr, and — since a report was staged for a
// bundle-carrying manifest and the node will be waiting for one — writes an explicit
// outcome=error report itself, so the node fails fast instead of stalling out the 2-minute
// AwaitApplyReport timeout (sp-mwco.2.10).
func TestApplyArtifacts_OldImageGuard(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	artifactsDir := t.TempDir()

	// Write a manifest so we don't hit the "no manifest" early-exit.
	manifest := `{"artifacts":[{"kind":"mcp","name":"test-mcp","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	// PATH has NO agentinstall — deliberately omit binDir.
	env := []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin", // no agentinstall here
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code != 1 {
		t.Fatalf("expected exit 1 for old-image guard, got %d\noutput:\n%s", code, out)
	}
	// Diagnostic message should mention agentinstall or old image.
	if !strings.Contains(out, "agentinstall") {
		t.Errorf("expected diagnostic mentioning agentinstall, got:\n%s", out)
	}

	// No config written (guard fired before invoking agentinstall).
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf("unexpected write to ~/.claude.json when guard fired: err=%v", err)
	}

	// The script itself now writes an explicit error report — the node must not stall waiting
	// for one that will never come from a CLI that was never invoked.
	reportPath := filepath.Join(artifactsDir, "report", "apply-report.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("expected apply-report.json to be written by the old-image guard: %v", err)
	}
	rep, err := spec.ParseApplyReport(data)
	if err != nil {
		t.Fatalf("parse apply-report.json: %v\ncontent: %s", err, data)
	}
	if rep.Schema != 1 {
		t.Errorf("schema: got %d want 1", rep.Schema)
	}
	if rep.Outcome != spec.OutcomeError {
		t.Errorf("outcome: got %q want %q", rep.Outcome, spec.OutcomeError)
	}
	if rep.Runnable != "claude-tui" {
		t.Errorf("runnable: got %q want claude-tui", rep.Runnable)
	}
	if !strings.Contains(rep.Error, "agentinstall") {
		t.Errorf("expected error to mention agentinstall, got %q", rep.Error)
	}
}

// TestApplyArtifacts_NoManifest verifies that the helper exits 0 silently when there is no
// manifest.json in the artifacts dir.
func TestApplyArtifacts_NoManifest(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	artifactsDir := t.TempDir() // deliberately empty — no manifest.json

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code != 0 {
		t.Fatalf("expected exit 0 for missing manifest, got %d\noutput:\n%s", code, out)
	}
	// No report either — agentinstall was never invoked (early-exit before dispatch).
	report := filepath.Join(artifactsDir, "report", "apply-report.json")
	if _, err := os.Stat(report); !os.IsNotExist(err) {
		t.Errorf("unexpected apply-report.json for missing manifest: err=%v", err)
	}
}

// stubAgentinstall writes a fake `agentinstall` executable to dir that runs script (a shell
// snippet) instead of the real CLI — used to simulate agentinstall exiting non-zero or dying by
// signal WITHOUT writing a report, exercising apply-artifacts.sh's EXIT-trap invariant
// (sp-mwco.2.12 ITEM B) independent of the real CLI's own report-writing behavior.
func stubAgentinstall(t *testing.T, dir, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "agentinstall"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestApplyArtifacts_NonZeroExitNoReport_WritesErrorReport is T1a: agentinstall exits non-zero
// WITHOUT writing a report (e.g. an unwritable --report path it never got to, or a crash before
// its own report write) — the residual "ITEM B" hole this task closes. Before this fix, the
// script's post-check only fired when rc==0, so a non-zero rc with no report left the node to
// burn its entire wait budget. Now the EXIT trap catches it regardless of rc.
func TestApplyArtifacts_NonZeroExitNoReport_WritesErrorReport(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	stubAgentinstall(t, binDir, "exit 7")

	artifactsDir := t.TempDir()
	manifest := `{"artifacts":[{"kind":"mcp","name":"test-mcp","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code != 7 {
		t.Fatalf("expected exit 7 (propagated from the stub), got %d\noutput:\n%s", code, out)
	}

	report := filepath.Join(artifactsDir, "report", "apply-report.json")
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("expected apply-report.json to be written by the EXIT trap: %v", err)
	}
	rep, err := spec.ParseApplyReport(data)
	if err != nil {
		t.Fatalf("parse apply-report.json: %v\ncontent: %s", err, data)
	}
	if rep.Outcome != spec.OutcomeError {
		t.Errorf("outcome: got %q want %q", rep.Outcome, spec.OutcomeError)
	}
	if !strings.Contains(rep.Error, "rc=7") {
		t.Errorf("expected error to mention rc=7, got %q", rep.Error)
	}
}

// TestApplyArtifacts_KilledBySignalNoReport_WritesErrorReport is T1b: agentinstall is killed by a
// signal (the subprocess self-signals, simulating an OOM kill or a crash) without writing a
// report — the parent script sees a 128+signal exit code and the EXIT trap must still catch it.
func TestApplyArtifacts_KilledBySignalNoReport_WritesErrorReport(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	stubAgentinstall(t, binDir, "kill -TERM $$; sleep 1")

	artifactsDir := t.TempDir()
	manifest := `{"artifacts":[{"kind":"mcp","name":"test-mcp","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code == 0 {
		t.Fatalf("expected non-zero exit (agentinstall killed by TERM), got 0\noutput:\n%s", out)
	}

	report := filepath.Join(artifactsDir, "report", "apply-report.json")
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("expected apply-report.json to be written by the EXIT trap: %v", err)
	}
	rep, err := spec.ParseApplyReport(data)
	if err != nil {
		t.Fatalf("parse apply-report.json: %v\ncontent: %s", err, data)
	}
	if rep.Outcome != spec.OutcomeError {
		t.Errorf("outcome: got %q want %q", rep.Outcome, spec.OutcomeError)
	}
}

// TestApplyArtifacts_UnwritableReportDir_IsLoud is T2: when the report/ path cannot be created
// (here: it collides with an existing plain FILE, so mkdir -p fails), write_error_report must not
// swallow the failure — it prints a grep-able SPAWNERY-APPLY-FATAL: marker to stderr, and the
// script still exits non-zero (via the old-image guard's own explicit exit 1, independent of
// whether the report write itself succeeded).
func TestApplyArtifacts_UnwritableReportDir_IsLoud(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()

	artifactsDir := t.TempDir()
	// report/ collides with a plain file, so `mkdir -p "$ARTIFACTS_DIR/report"` fails.
	if err := os.WriteFile(filepath.Join(artifactsDir, "report"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	// PATH has NO agentinstall — the old-image guard fires first (before the manifest/report
	// dispatch), independent of the manifest state.
	env := []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code == 0 {
		t.Fatalf("expected non-zero exit, got 0\noutput:\n%s", out)
	}
	if !strings.Contains(out, "SPAWNERY-APPLY-FATAL:") {
		t.Fatalf("expected stderr to contain SPAWNERY-APPLY-FATAL: marker, got:\n%s", out)
	}
}

// TestApplyArtifacts_PropagatesNonZeroExit verifies that a manifest whose only artifact fails to
// apply (a bogus emitter mapping means the artifact will be reported skipped/failed, not that
// the shell wrapper swallows it to 0) makes apply-artifacts.sh itself exit non-zero — the
// "always exit 0" contract is gone; the exit code is now agentinstall's own (propagated).
func TestApplyArtifacts_PropagatesNonZeroExit(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	artifactsDir := t.TempDir()
	skillDir := filepath.Join(artifactsDir, "payloads", "s1")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# s1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two members of the same bundle_ref group; s2's payload dir is missing, so it fails to
	// apply — the all-or-nothing bundle contract makes this bundle_failed (exit 3).
	manifest := `{"artifacts":[
		{"kind":"skill","name":"s1","targets":["claude"],"bundle":"b1","skill":{"dir":"payloads/s1"}},
		{"kind":"skill","name":"s2","targets":["claude"],"bundle":"b1","skill":{"dir":"payloads/missing"}}
	]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
		"SECRET_WAIT_TIMEOUT=1s",
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code != 3 {
		t.Fatalf("expected exit 3 (bundle_failed) propagated from agentinstall, got %d\noutput:\n%s", code, out)
	}

	report := filepath.Join(artifactsDir, "report", "apply-report.json")
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("apply-report.json not written: %v", err)
	}
	if !strings.Contains(string(data), `"outcome":"bundle_failed"`) {
		t.Errorf("expected bundle_failed outcome in report, got: %s", data)
	}
}

// TestApplyArtifacts_SecretWaitRoundTrip verifies that a pre-written secret file is picked up
// by agentinstall and the value lands in ~/.claude.json.
func TestApplyArtifacts_SecretWaitRoundTrip(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	artifactsDir := t.TempDir()
	secretsDir := t.TempDir()

	// Write the secret file BEFORE running the helper (simulates a pre-start sync delivery).
	if err := os.WriteFile(filepath.Join(secretsDir, "MY_TOKEN"), []byte("s3cr3t-value"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Manifest: claude MCP with a secretRef.
	manifest := `{"artifacts":[{"kind":"mcp","name":"secret-mcp","targets":["claude"],"sensitive":true,"mcp":{"stdio":{"command":"npx"},"secretRefs":["MY_TOKEN"]}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + secretsDir,
		"SECRET_WAIT_TIMEOUT=5s",
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\noutput:\n%s", code, out)
	}

	// ~/.claude.json must carry the secret value in the env map.
	claudeJSON := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(claudeJSON)
	if err != nil {
		t.Fatalf("~/.claude.json not written: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse .claude.json: %v\ncontent: %s", err, data)
	}
	servers, _ := root["mcpServers"].(map[string]interface{})
	server, _ := servers["secret-mcp"].(map[string]interface{})
	envMap, _ := server["env"].(map[string]interface{})
	if envMap["MY_TOKEN"] != "s3cr3t-value" {
		t.Errorf("MY_TOKEN not injected: got %v\n.claude.json: %s", envMap["MY_TOKEN"], data)
	}

	// File should be 0600 (secrets present → filePerm returns 0600).
	fi, err := os.Stat(claudeJSON)
	if err != nil {
		t.Fatalf("stat .claude.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm: got %o, want 0600", fi.Mode().Perm())
	}
}

// --- `agentinstall apply --report` exit-code + envelope contract -----------------------------
//
// These exercise the CLI binary directly (not through apply-artifacts.sh) since the --report
// exit-code contract (0/2/3/1) is a CLI concern the shell wrapper simply propagates.

// applyReportEnvelope is a minimal decode of the apply-report.json schema for assertions.
type applyReportEnvelope struct {
	Schema  int    `json:"schema"`
	Agent   string `json:"agent"`
	Outcome string `json:"outcome"`
	Error   string `json:"error"`
	Bundles []struct {
		Bundle   string `json:"bundle"`
		Targeted int    `json:"targeted"`
		Applied  int    `json:"applied"`
		Failed   int    `json:"failed"`
		Skipped  int    `json:"skipped"`
	} `json:"bundles"`
	Reports []struct {
		Agent  string `json:"agent"`
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Bundle string `json:"bundle"`
	} `json:"reports"`
}

func runApply(t *testing.T, bin, home string, args ...string) (string, int) {
	t.Helper()
	full := append([]string{"apply"}, args...)
	cmd := exec.Command(bin, full...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run apply: %v", err)
		}
	}
	return string(out), code
}

func readApplyReport(t *testing.T, path string) applyReportEnvelope {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report %s: %v", path, err)
	}
	var env applyReportEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("parse report %s: %v\ncontent: %s", path, err, data)
	}
	return env
}

// TestApplyReport_AllApplied_ExitZero verifies a fully-applied manifest writes outcome=ok and
// exits 0 with --report set.
func TestApplyReport_AllApplied_ExitZero(t *testing.T) {
	binDir := t.TempDir()
	bin := buildAgentinstallToDir(t, binDir)
	home := t.TempDir()

	stagingDir := t.TempDir()
	manifest := `{"artifacts":[{"kind":"mcp","name":"m1","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(stagingDir, "report", "apply-report.json")

	_, code := runApply(t, bin, home, "--artifacts", stagingDir, "--agent", "claude", "--report", reportPath)
	if code != 0 {
		t.Fatalf("exit code: got %d want 0", code)
	}
	env := readApplyReport(t, reportPath)
	if env.Outcome != "ok" || env.Schema != 1 {
		t.Errorf("envelope: %+v", env)
	}
}

// TestApplyReport_BundleMemberFailed_ExitThree verifies a manifest with a bundle whose member
// targets an unregistered agent (producing a skipped entry) writes outcome=bundle_failed and
// exits 3 (the all-or-nothing bundle contract).
func TestApplyReport_BundleMemberFailed_ExitThree(t *testing.T) {
	binDir := t.TempDir()
	bin := buildAgentinstallToDir(t, binDir)
	home := t.TempDir()

	stagingDir := t.TempDir()
	skillDir := filepath.Join(stagingDir, "payloads", "s1")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# s1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// s1 applies for claude; s2 is the same bundle_ref group but its payload dir does not
	// exist (only payloads/s1 was created below) — a real per-member install failure inside
	// one bundle_ref group, tripping the all-or-nothing verdict.
	manifest := `{"artifacts":[
		{"kind":"skill","name":"s1","targets":["claude"],"bundle":"b1","skill":{"dir":"payloads/s1"}},
		{"kind":"skill","name":"s2","targets":["claude"],"bundle":"b1","skill":{"dir":"payloads/missing"}}
	]}`
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	// s2's payload dir is intentionally missing (only payloads/s1 was created), so InstallSkill
	// fails for it — a real per-member failure inside one bundle_ref group.
	reportPath := filepath.Join(stagingDir, "report", "apply-report.json")

	_, code := runApply(t, bin, home, "--artifacts", stagingDir, "--agent", "claude", "--report", reportPath)
	if code != 3 {
		t.Fatalf("exit code: got %d want 3", code)
	}
	env := readApplyReport(t, reportPath)
	if env.Outcome != "bundle_failed" {
		t.Fatalf("outcome: got %q want bundle_failed; envelope: %+v", env.Outcome, env)
	}
	if len(env.Bundles) != 1 || env.Bundles[0].Bundle != "b1" || env.Bundles[0].Targeted != 2 || env.Bundles[0].Applied != 1 {
		t.Fatalf("bundle rollup: %+v", env.Bundles)
	}
}

// TestApplyReport_ManifestLoadError_ExitOne verifies a missing manifest.json writes
// outcome=error to the report and exits 1.
func TestApplyReport_ManifestLoadError_ExitOne(t *testing.T) {
	binDir := t.TempDir()
	bin := buildAgentinstallToDir(t, binDir)
	home := t.TempDir()

	stagingDir := t.TempDir() // no manifest.json
	reportPath := filepath.Join(stagingDir, "report", "apply-report.json")

	_, code := runApply(t, bin, home, "--artifacts", stagingDir, "--agent", "claude", "--report", reportPath)
	if code != 1 {
		t.Fatalf("exit code: got %d want 1", code)
	}
	env := readApplyReport(t, reportPath)
	if env.Outcome != "error" || env.Error == "" {
		t.Fatalf("envelope: %+v", env)
	}
}

// TestApplyReport_WrittenAtomically verifies the report dir contains exactly the final file (no
// leftover .tmp artifacts from the atomic write).
func TestApplyReport_WrittenAtomically(t *testing.T) {
	binDir := t.TempDir()
	bin := buildAgentinstallToDir(t, binDir)
	home := t.TempDir()

	stagingDir := t.TempDir()
	manifest := `{"artifacts":[{"kind":"mcp","name":"m1","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	reportDir := filepath.Join(stagingDir, "report")
	reportPath := filepath.Join(reportDir, "apply-report.json")

	if _, code := runApply(t, bin, home, "--artifacts", stagingDir, "--agent", "claude", "--report", reportPath); code != 0 {
		t.Fatalf("exit code: got %d want 0", code)
	}
	entries, err := os.ReadDir(reportDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "apply-report.json" {
		t.Fatalf("report dir should contain exactly apply-report.json, got %v", entries)
	}
}

// TestApplyWithoutReport_Unchanged is a regression guard: omitting --report must leave stdout
// and exit code exactly as before this task (always 0, no report file written).
func TestApplyWithoutReport_Unchanged(t *testing.T) {
	binDir := t.TempDir()
	bin := buildAgentinstallToDir(t, binDir)
	home := t.TempDir()

	stagingDir := t.TempDir()
	manifest := `{"artifacts":[{"kind":"mcp","name":"m1","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := runApply(t, bin, home, "--artifacts", stagingDir, "--agent", "claude")
	if code != 0 {
		t.Fatalf("exit code: got %d want 0", code)
	}
	var result struct {
		Reports []struct {
			Status string `json:"status"`
		} `json:"reports"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &result); err != nil {
		t.Fatalf("stdout not the plain Result JSON: %v\noutput: %s", err, out)
	}
	if len(result.Reports) != 1 || result.Reports[0].Status != "applied" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(stagingDir, "apply-report.json")); !os.IsNotExist(err) {
		t.Errorf("no --report flag should not write a report file: err=%v", err)
	}
}
