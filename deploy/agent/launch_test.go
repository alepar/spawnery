package agent_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stubBin writes an executable script named `name` into `dir`, for tests that put `dir` on
// PATH ahead of the system PATH to intercept a command the launcher shells out to.
func stubBin(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLauncherRepairsJSONLByTruncatingAtFirstInvalidLine(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeJQ := `#!/bin/sh
input=$(cat)
case "$input" in
  '{"turn":1}'|'{"turn":2}') exit 0 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "jq"), []byte(fakeJQ), 0o755); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(dir, "projects", "fork")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(projectDir, "session.jsonl")
	before := "{\"turn\":1}\n{\"turn\":2}\n{\"turn\":"
	if err := os.WriteFile(sessionFile, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", "set --; . ./launch; repair_jsonl_tree \"$SPAWNERY_JSONL_TEST_DIR\"")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SPAWNERY_LAUNCH_SOURCE_ONLY=1",
		"SPAWNERY_JSONL_TEST_DIR="+dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("repair_jsonl_tree failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\"turn\":1}\n{\"turn\":2}\n"; string(got) != want {
		t.Fatalf("repaired JSONL = %q, want %q", got, want)
	}
}

func TestLauncherWiresResumeAfterJSONLRepair(t *testing.T) {
	data, err := os.ReadFile("launch")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, snippet := range []string{
		"repair_jsonl_tree \"$CLAUDE_PROJECTS_DIR\"",
		"set -- \"$@\" --continue",
		"repair_jsonl_tree \"$CODEX_HOME\"",
		"set -- \"$@\" resume --last",
	} {
		if !strings.Contains(script, snippet) {
			t.Fatalf("launcher missing %q", snippet)
		}
	}
}

// TestInstallSpawnCAWithUpdater verifies that install_spawn_ca copies spawn-ca.crt to the
// Debian anchor dir and invokes update-ca-certificates when the latter is on PATH.
func TestInstallSpawnCAWithUpdater(t *testing.T) {
	dir := t.TempDir()

	// Create a fake git-env dir with spawn-ca.crt.
	gitEnvDir := filepath.Join(dir, "git-env")
	if err := os.MkdirAll(gitEnvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	certContent := "-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(filepath.Join(gitEnvDir, "spawn-ca.crt"), []byte(certContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a fake update-ca-certificates that records its invocation.
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(dir, "updater-called")
	fakeUpdater := "#!/bin/sh\ntouch " + invocationFile + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "update-ca-certificates"), []byte(fakeUpdater), 0o755); err != nil {
		t.Fatal(err)
	}

	// SPAWNERY_CA_ANCHOR_DIR redirects the anchor dir to a temp path (test seam).
	anchorBase := filepath.Join(dir, "ca-anchors")
	if err := os.MkdirAll(anchorBase, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", "set --; . ./launch; install_spawn_ca \"$GITENV_DIR\"")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SPAWNERY_LAUNCH_SOURCE_ONLY=1",
		"GITENV_DIR="+gitEnvDir,
		"SPAWNERY_CA_ANCHOR_DIR="+anchorBase,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install_spawn_ca failed: %v\n%s", err, out)
	}

	// The cert must have been copied to <anchorBase>/spawnery/spawnery-spawn.crt.
	copied, err := os.ReadFile(filepath.Join(anchorBase, "spawnery", "spawnery-spawn.crt"))
	if err != nil {
		t.Fatalf("cert not copied to anchor dir: %v", err)
	}
	if string(copied) != certContent {
		t.Errorf("copied cert = %q, want %q", copied, certContent)
	}

	// update-ca-certificates must have been invoked.
	if _, err := os.Stat(invocationFile); err != nil {
		t.Fatal("update-ca-certificates was not called")
	}
}

// TestInstallSpawnCANoUpdater verifies that install_spawn_ca exits 0 (best-effort) when no
// CA updater is present on PATH.
func TestInstallSpawnCANoUpdater(t *testing.T) {
	dir := t.TempDir()

	gitEnvDir := filepath.Join(dir, "git-env")
	if err := os.MkdirAll(gitEnvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitEnvDir, "spawn-ca.crt"), []byte("FAKE"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Empty bin dir — no update-ca-certificates / update-ca-trust.
	binDir := filepath.Join(dir, "empty-bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", "set --; . ./launch; install_spawn_ca \"$GITENV_DIR\"")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir, // no system path so system updaters are hidden
		"SPAWNERY_LAUNCH_SOURCE_ONLY=1",
		"GITENV_DIR="+gitEnvDir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install_spawn_ca should exit 0 even without an updater: %v\n%s", err, out)
	}
}

// TestLauncherAppliesArtifactsGooseACP verifies the goose-acp branch calls apply-artifacts
// before its terminal exec. The branch's terminal command is an absolute path
// (/usr/local/bin/acpmux) that does not exist on the test host, so `sh` exits 127 once it gets
// there — *after* apply-artifacts already ran. We assert on that ordering, not on exit 0.
func TestLauncherAppliesArtifactsGooseACP(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordFile := filepath.Join(dir, "record")
	stubBin(t, binDir, "apply-artifacts", "#!/bin/sh\nprintf '%s' \"$1\" > \""+recordFile+"\"\n")

	cmd := exec.Command("sh", "./launch", "--runnable", "goose-acp")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected launch to fail exec'ing the nonexistent acpmux, got success:\n%s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an ExitError, got %v\n%s", err, out)
	}
	if exitErr.ExitCode() != 127 {
		t.Fatalf("exit code = %d, want 127 (missing acpmux)\n%s", exitErr.ExitCode(), out)
	}

	got, rerr := os.ReadFile(recordFile)
	if rerr != nil {
		t.Fatalf("apply-artifacts was not called: %v\n%s", rerr, out)
	}
	if string(got) != "goose-acp" {
		t.Fatalf("apply-artifacts called with %q, want %q", got, "goose-acp")
	}
}

// TestLauncherAppliesArtifactsHermesACPAfterConfigWrite verifies the hermes-acp branch calls
// apply-artifacts, and that the call happens AFTER the launcher writes
// $HERMES_HOME/config.yaml. This ordering is load-bearing: the future hermes skills emitter
// (sp-mwco.2.5) upserts skills.external_dirs into that same file — calling apply-artifacts
// before the heredoc write would let the launcher silently clobber the emitter's edit.
func TestLauncherAppliesArtifactsHermesACPAfterConfigWrite(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordFile := filepath.Join(dir, "record")
	hermesHome := filepath.Join(dir, "hermes-home")
	script := `#!/bin/sh
{
  printf '%s' "$1"
  if [ -f "$HERMES_HOME/config.yaml" ]; then
    printf ':config-exists'
  fi
} > "` + recordFile + `"
`
	stubBin(t, binDir, "apply-artifacts", script)

	cmd := exec.Command("sh", "./launch", "--runnable", "hermes-acp")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HERMES_HOME="+hermesHome,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected launch to fail exec'ing the nonexistent acpmux, got success:\n%s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an ExitError, got %v\n%s", err, out)
	}
	if exitErr.ExitCode() != 127 {
		t.Fatalf("exit code = %d, want 127 (missing acpmux)\n%s", exitErr.ExitCode(), out)
	}

	got, rerr := os.ReadFile(recordFile)
	if rerr != nil {
		t.Fatalf("apply-artifacts was not called: %v\n%s", rerr, out)
	}
	if want := "hermes-acp:config-exists"; string(got) != want {
		t.Fatalf("apply-artifacts record = %q, want %q (call must happen AFTER the config.yaml write)", got, want)
	}
}

// TestLauncherAppliesArtifactsGooseTUI verifies the goose-tui branch calls apply-artifacts
// before handing off to start_tmux. With a stubbed tmux (exit 0) and KEEPALIVE unset, the
// script returns 0 right after start_tmux, so a clean exit plus the recorded call together
// prove the call landed before the tmux handoff.
func TestLauncherAppliesArtifactsGooseTUI(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordFile := filepath.Join(dir, "record")
	stubBin(t, binDir, "apply-artifacts", "#!/bin/sh\nprintf '%s' \"$1\" > \""+recordFile+"\"\n")
	stubBin(t, binDir, "tmux", "#!/bin/sh\nexit 0\n")

	cmd := exec.Command("sh", "./launch", "--runnable", "goose-tui")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launch failed: %v\n%s", err, out)
	}

	got, rerr := os.ReadFile(recordFile)
	if rerr != nil {
		t.Fatalf("apply-artifacts was not called: %v\n%s", rerr, out)
	}
	if string(got) != "goose-tui" {
		t.Fatalf("apply-artifacts called with %q, want %q", got, "goose-tui")
	}
}

// TestLauncherContinuesWhenApplyArtifactsFailsButReportExists verifies the sp-mwco.2.12 rule: a
// non-zero apply-artifacts exit does NOT abort the launcher when a report was written — the node,
// not the launcher, owns the install verdict. Uses goose-tui (start_tmux path) so a clean overall
// exit proves the launcher proceeded past apply_artifacts despite its failure.
func TestLauncherContinuesWhenApplyArtifactsFailsButReportExists(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifactsDir := filepath.Join(dir, "artifacts")
	reportDir := filepath.Join(artifactsDir, "report")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reportDir, "apply-report.json"), []byte(`{"schema":1,"outcome":"error","reports":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stubBin(t, binDir, "apply-artifacts", "#!/bin/sh\nexit 3\n")
	stubBin(t, binDir, "tmux", "#!/bin/sh\nexit 0\n")

	cmd := exec.Command("sh", "./launch", "--runnable", "goose-tui")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SPAWNERY_ARTIFACTS_DIR="+artifactsDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launch should continue past a reported apply-artifacts failure, got: %v\n%s", err, out)
	}
}

// TestLauncherAbortsWhenApplyArtifactsFailsWithNoReport verifies the sp-mwco.2.12 rule: a
// non-zero apply-artifacts exit WITHOUT a report aborts the launcher (the only remaining signal
// the node can observe is the container dying) with a grep-able stderr marker.
func TestLauncherAbortsWhenApplyArtifactsFailsWithNoReport(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifactsDir := filepath.Join(dir, "artifacts") // deliberately no report/ subdir created
	stubBin(t, binDir, "apply-artifacts", "#!/bin/sh\nexit 3\n")
	stubBin(t, binDir, "tmux", "#!/bin/sh\nexit 0\n")

	cmd := exec.Command("sh", "./launch", "--runnable", "goose-tui")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SPAWNERY_ARTIFACTS_DIR="+artifactsDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected launch to abort on a report-less apply-artifacts failure, got success:\n%s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an ExitError, got %v\n%s", err, out)
	}
	if exitErr.ExitCode() != 3 {
		t.Fatalf("exit code = %d, want 3 (propagated from apply-artifacts)\n%s", exitErr.ExitCode(), out)
	}
	if !strings.Contains(string(out), "SPAWNERY-APPLY-FATAL:") {
		t.Fatalf("expected stderr to contain SPAWNERY-APPLY-FATAL: marker, got:\n%s", out)
	}
	// tmux must never have been reached — the launcher aborted before start_tmux.
	if strings.Contains(string(out), "goose session") {
		t.Fatalf("launcher should have aborted before reaching start_tmux:\n%s", out)
	}
}

// TestLauncherCallsApplyArtifactsInEveryAgentBranch is a regression guard: it parses the case
// statement in `launch` and asserts every agent runnable branch calls apply-artifacts BEFORE
// its terminal exec/start_tmux, and that non-agent branches (shell, stub-acp, nori) do not call
// it at all. This is what keeps a future branch from repeating the goose/hermes bug.
func TestLauncherCallsApplyArtifactsInEveryAgentBranch(t *testing.T) {
	data, err := os.ReadFile("launch")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")

	// Case labels are indented exactly two spaces, with a non-space third column (this excludes
	// four-space-indented comment lines inside branch bodies that happen to end in ')').
	labelRe := regexp.MustCompile(`^  \S.*\)$`)

	agentBranches := []string{
		`""|opencode-served)`,
		`goose-acp)`,
		`hermes-acp)`,
		`opencode-tui)`,
		`goose-tui)`,
		`claude-tui)`,
		`codex-tui)`,
		`pi-tui)`,
		`pi-acp)`,
	}
	nonAgentBranches := []string{
		`shell)`,
		`stub-acp)`,
		`nori*|nori)`,
	}
	isAgentBranch := map[string]bool{}
	for _, b := range agentBranches {
		isAgentBranch[b] = true
	}
	isNonAgentBranch := map[string]bool{}
	for _, b := range nonAgentBranches {
		isNonAgentBranch[b] = true
	}

	seen := map[string]bool{}
	var label string
	var body []string

	flush := func() {
		if label == "" {
			return
		}
		seen[label] = true
		joined := strings.Join(body, "\n")
		aaIdx := strings.Index(joined, "apply_artifacts")
		termIdx := -1
		for _, term := range []string{"exec ", "start_tmux "} {
			if idx := strings.Index(joined, term); idx != -1 && (termIdx == -1 || idx < termIdx) {
				termIdx = idx
			}
		}
		switch {
		case isAgentBranch[label]:
			if aaIdx == -1 {
				t.Errorf("agent branch %q: missing apply-artifacts call", label)
			} else if termIdx != -1 && aaIdx > termIdx {
				t.Errorf("agent branch %q: apply-artifacts called after exec/start_tmux (aaIdx=%d termIdx=%d)", label, aaIdx, termIdx)
			}
		case isNonAgentBranch[label]:
			if aaIdx != -1 {
				t.Errorf("non-agent branch %q: unexpectedly calls apply-artifacts", label)
			}
		}
	}

	for _, line := range lines {
		if labelRe.MatchString(line) {
			flush()
			label = strings.TrimSpace(line)
			body = nil
			continue
		}
		if strings.TrimSpace(line) == ";;" {
			flush()
			label = ""
			body = nil
			continue
		}
		if label != "" {
			body = append(body, line)
		}
	}
	flush()

	for _, b := range agentBranches {
		if !seen[b] {
			t.Errorf("agent branch %q not found in launch script (script structure changed?)", b)
		}
	}
	for _, b := range nonAgentBranches {
		if !seen[b] {
			t.Errorf("non-agent branch %q not found in launch script (script structure changed?)", b)
		}
	}
}
