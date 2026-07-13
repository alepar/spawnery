//go:build e2e

package cp_test

// checkAgentImageFresh / preflightAgentImageFresh guard against the sp-mwco.2.11 false-P0: a
// spawnery/agent:dev image built from a stale checkout (e.g. master, or an older commit on this
// branch) silently writes apply-report.json to the wrong path, exits 0, and the node then times
// out after ~2 minutes on "apply-report.json was not written before the deadline" — a misleading
// error that looks like a product bug but is really an out-of-date local Docker image.
//
// The check compares the sha256 of /usr/local/bin/apply-artifacts INSIDE the image against the
// sha256 of deploy/agent/apply-artifacts.sh ON DISK in the current worktree (the Dockerfile COPYs
// that script verbatim with no rewriting — deploy/agent/Dockerfile:182/193 — so a byte compare is
// exact). This is strictly stronger than grepping the image for one staleness marker string: it
// catches ANY drift between the image and the branch under test, not just the one known-bad
// marker, and it reproduces in code exactly the md5 comparison that resolved the original P0.
//
// See bd show sp-mwco.2.11 for full history.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// checkAgentImageFresh compares the sha256 of /usr/local/bin/apply-artifacts inside image against
// the sha256 of the file at scriptPath on disk. It returns nil when they match, and a detailed
// error (naming the image, both short hashes, and the `make images` fix) when they don't — or
// when the image can't be inspected (e.g. it predates apply-artifacts existing at all).
func checkAgentImageFresh(image, scriptPath string) error {
	onDisk, err := os.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("checkAgentImageFresh: read %s: %w", scriptPath, err)
	}
	wantSum := sha256.Sum256(onDisk)
	wantHex := hex.EncodeToString(wantSum[:])

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--entrypoint", "sha256sum",
		image, "/usr/local/bin/apply-artifacts").CombinedOutput()
	if err != nil {
		return fmt.Errorf("agent image %s does not contain /usr/local/bin/apply-artifacts at all "+
			"(image predates the apply-artifacts script — even older than the sp-mwco.2.7 staleness "+
			"this preflight normally catches). Rebuild from THIS worktree: cd <repo> && make images\n"+
			"docker error: %v\n%s", image, err, out)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return fmt.Errorf("agent image %s: could not parse sha256sum output: %q", image, out)
	}
	gotHex := fields[0]

	if gotHex == wantHex {
		return nil
	}
	return fmt.Errorf(
		"agent image %s is STALE: /usr/local/bin/apply-artifacts sha256=%s but this branch's "+
			"deploy/agent/apply-artifacts.sh is sha256=%s. The image predates sp-mwco.2.7 "+
			"(report/apply-report.json + --report). A stale script writes the report to the WRONG "+
			"path, exits 0, and the node then times out for 2 minutes with a misleading "+
			`"apply-report.json was not written" error. Rebuild from THIS worktree: cd <repo> && make images`,
		image, shortHash(gotHex), shortHash(wantHex))
}

// shortHash truncates a hex digest to 12 characters for legible error messages.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// preflightAgentImageFresh fails the test loudly (not a skip — build-tag-is-opt-in) when
// spawnery/agent:dev's apply-artifacts script does not match this branch's
// deploy/agent/apply-artifacts.sh, converting a 2-minute install-report timeout into an instant,
// legible diagnosis.
func preflightAgentImageFresh(t *testing.T) {
	t.Helper()
	scriptPath := filepath.Join("..", "..", "deploy", "agent", "apply-artifacts.sh")
	if err := checkAgentImageFresh("spawnery/agent:dev", scriptPath); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestPreflightAgentImageFreshDetectsStale is the self-test for checkAgentImageFresh: it builds a
// throwaway "stale" overlay image (spawnery/agent:dev + a deliberately different
// apply-artifacts script) and asserts the freshness check rejects it with a legible message,
// while spawnery/agent:dev itself (the positive control) passes.
func TestPreflightAgentImageFreshDetectsStale(t *testing.T) {
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Fatalf("Docker is required: %v\n%s", err, out)
	}

	scriptPath := filepath.Join("..", "..", "deploy", "agent", "apply-artifacts.sh")
	onDisk, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}

	// Positive control first: if spawnery/agent:dev itself is stale, everything below is moot —
	// fail with the real diagnosis rather than a confusing self-test failure.
	if err := checkAgentImageFresh("spawnery/agent:dev", scriptPath); err != nil {
		t.Fatalf("positive control failed — spawnery/agent:dev itself is stale, rebuild it first (`make images`): %v", err)
	}

	// Build a throwaway overlay tagged over a *copy* name (never spawnery/agent:dev itself) whose
	// apply-artifacts script is deliberately different from the on-disk one — stripping the
	// "report/" path segment is sufficient to produce different bytes without needing a real
	// master checkout.
	staleScript := strings.Replace(string(onDisk), "report/apply-report.json", "apply-report.json", 1)
	if staleScript == string(onDisk) {
		t.Fatalf("test setup: expected deploy/agent/apply-artifacts.sh to contain %q so the stale "+
			"variant differs from it — the marker string may have changed, update this test", "report/apply-report.json")
	}

	buildCtx := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildCtx, "apply-artifacts.sh"), []byte(staleScript), 0o644); err != nil {
		t.Fatalf("write stale script: %v", err)
	}
	dockerfile := "FROM spawnery/agent:dev\nCOPY apply-artifacts.sh /usr/local/bin/apply-artifacts\n"
	if err := os.WriteFile(filepath.Join(buildCtx, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	staleTag := fmt.Sprintf("spawnery/agent:stale-preflight-test-%d", os.Getpid())
	buildCtxTimeout, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(buildCtxTimeout, "docker", "build", "-t", staleTag, buildCtx).CombinedOutput(); err != nil {
		t.Fatalf("docker build stale test image: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rmi", "-f", staleTag).Run()
	})

	err = checkAgentImageFresh(staleTag, scriptPath)
	if err == nil {
		t.Fatalf("checkAgentImageFresh(%q, ...) = nil, want a staleness error", staleTag)
	}
	msg := err.Error()
	for _, want := range []string{staleTag, "make images"} {
		if !strings.Contains(msg, want) {
			t.Errorf("checkAgentImageFresh error missing %q in message: %s", want, msg)
		}
	}
	wantSum := sha256.Sum256(onDisk)
	wantShort := shortHash(hex.EncodeToString(wantSum[:]))
	staleSum := sha256.Sum256([]byte(staleScript))
	staleShort := shortHash(hex.EncodeToString(staleSum[:]))
	for _, wantHash := range []string{wantShort, staleShort} {
		if !strings.Contains(msg, wantHash) {
			t.Errorf("checkAgentImageFresh error missing short hash %q in message: %s", wantHash, msg)
		}
	}
}
