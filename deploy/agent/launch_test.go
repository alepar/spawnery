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
