package agent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
