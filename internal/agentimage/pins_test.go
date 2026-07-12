package agentimage

import (
	"os"
	"strings"
	"testing"
)

// dockerfilePath resolves deploy/agent/Dockerfile relative to this package's directory
// (go test's working directory is always the package dir).
const dockerfilePath = "../../deploy/agent/Dockerfile"

// pinnedAgentARGs are the ARGs that MUST name a concrete, non-floating version. The other
// pinned-but-unrelated ARGs in the Dockerfile (GH_VERSION, NODE_VERSION) are out of scope for
// this bead — it only asserts the six agent binaries.
var pinnedAgentARGs = []string{
	"GOOSE_VERSION",
	"CLAUDE_CODE_VERSION",
	"OPENCODE_VERSION",
	"CODEX_VERSION",
	"PI_VERSION",
	"HERMES_VERSION",
}

// TestAgentBinariesArePinned is the durable regression guard: it fails in the default `just
// test` suite, with no Docker, the moment anyone re-floats a pin (removes the ARG, empties its
// default, or points it at a non-semver channel like "stable"/"latest"/"main").
func TestAgentBinariesArePinned(t *testing.T) {
	pins, err := Pins(dockerfilePath)
	if err != nil {
		t.Fatalf("Pins(%s): %v", dockerfilePath, err)
	}

	for _, name := range pinnedAgentARGs {
		def, ok := pins[name]
		if !ok {
			t.Errorf("ARG %s not found in %s", name, dockerfilePath)
			continue
		}
		if def == "" {
			t.Errorf("ARG %s has no default in %s (must be pinned)", name, dockerfilePath)
			continue
		}
		if SemverOf(def) == "" {
			t.Errorf("ARG %s=%q does not name a concrete version (SemverOf found none) — looks floating", name, def)
		}
	}
}

// TestNoFloatingRefsInDockerfile is a cheap textual guard that each pin ARG is not just declared
// but actually interpolated, and that no known floating-tag pattern remains in the file. An ARG
// nobody interpolates is the obvious way to fake TestAgentBinariesArePinned.
func TestNoFloatingRefsInDockerfile(t *testing.T) {
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read %s: %v", dockerfilePath, err)
	}
	body := string(data)

	if strings.Contains(body, "releases/download/stable/") {
		t.Errorf("%s references a floating GitHub `stable` release tag (releases/download/stable/) — pin to a concrete version", dockerfilePath)
	}

	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "apt-get install") && strings.Contains(line, "claude-code") && !strings.Contains(line, "claude-code=") {
			t.Errorf("%s installs claude-code without an exact version pin: %q", dockerfilePath, strings.TrimSpace(line))
		}
	}

	for _, name := range pinnedAgentARGs {
		if !strings.Contains(body, "${"+name+"}") {
			t.Errorf("ARG %s is declared but never interpolated (${%s}) in %s", name, name, dockerfilePath)
		}
	}
}

func TestSemverOf(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"v1.41.0", "1.41.0"},
		{"rust-v0.137.0", "0.137.0"},
		{"2.1.197-1", "2.1.197"},
		{"goose 1.41.0", "1.41.0"},
		{"2.1.197 (Claude Code)", "2.1.197"},
		{"none", ""},
		{"stable", ""},
		{"latest", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := SemverOf(c.in); got != c.want {
			t.Errorf("SemverOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
