//go:build e2e

package agentimage

import (
	"os/exec"
	"strings"
	"testing"
)

// binaryARG maps each agent binary's `--version` invocation to the Dockerfile ARG that pins it.
var binaryARG = map[string]string{
	"goose":    "GOOSE_VERSION",
	"claude":   "CLAUDE_CODE_VERSION",
	"opencode": "OPENCODE_VERSION",
	"codex":    "CODEX_VERSION",
	"pi":       "PI_VERSION",
	"hermes":   "HERMES_VERSION",
}

// TestAgentImageVersionsMatchPins proves the built spawnery/agent:dev image actually runs the
// binary versions the Dockerfile's ARG pins claim (sp-mwco.2.1). A mismatch here means the image
// was built against a moved floating tag (or a stale local image) — the epic's alarm bell for
// every "supported" capability claim downstream.
func TestAgentImageVersionsMatchPins(t *testing.T) {
	if out, err := exec.Command("docker", "image", "inspect", "spawnery/agent:dev").CombinedOutput(); err != nil {
		t.Fatalf("agent image missing: run `make images`: %v\n%s", err, out)
	}

	pins, err := Pins(dockerfilePath)
	if err != nil {
		t.Fatalf("Pins(%s): %v", dockerfilePath, err)
	}

	// Single container start: one `--version` per binary, each printed as "name=<raw output>"
	// (multi-line output is squashed to one line via tr — we only need the first semver token).
	const script = `
echo "goose=$(goose --version 2>&1 | tr '\n' ' ')"
echo "claude=$(claude --version 2>&1 | tr '\n' ' ')"
echo "opencode=$(opencode --version 2>&1 | tr '\n' ' ')"
echo "codex=$(codex --version 2>&1 | tr '\n' ' ')"
echo "pi=$(pi --version 2>&1 | tr '\n' ' ')"
echo "hermes=$(hermes --version 2>&1 | tr '\n' ' ')"
`
	out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "sh", "spawnery/agent:dev", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run version probe: %v\n%s", err, out)
	}

	actual := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		actual[name] = val
	}

	for name, argName := range binaryARG {
		rawActual, ok := actual[name]
		if !ok {
			t.Errorf("%s: no version output captured (probe script output:\n%s)", name, out)
			continue
		}
		pinned, ok := pins[argName]
		if !ok || pinned == "" {
			t.Fatalf("ARG %s not pinned in %s (TestAgentBinariesArePinned should already catch this)", argName, dockerfilePath)
		}

		wantSemver := SemverOf(pinned)
		gotSemver := SemverOf(rawActual)
		if wantSemver == "" || gotSemver == "" || wantSemver != gotSemver {
			t.Errorf("%s version mismatch: image reports %q (semver %q), Dockerfile pins %s=%q (semver %q)",
				name, strings.TrimSpace(rawActual), gotSemver, argName, pinned, wantSemver)
		}
	}
}
