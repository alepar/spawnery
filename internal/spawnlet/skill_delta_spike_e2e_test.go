//go:build skill_spike_e2e

package spawnlet

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"spawnery/internal/runtime"
)

// plantBothSkillTrees plants a canary tree (with a sentinel file, matching installSkillTree's
// 0700-chmod-on-the-skill-top-dir convention) under BOTH candidate roots, so one delta capture
// answers whether the image bakes both trees intact — the same reasoning the plan's D1 step lays
// out (suspend and fork commit the same top layer, so one CaptureDeltaAs answers both).
func (h *skillSpikeHarness) plantBothSkillTrees(ctx context.Context, spawnID, name, nonce string) {
	h.t.Helper()
	h.plantCanary(ctx, spawnID, "/root/.agents/skills", name, nonce, "", "")
	h.plantCanary(ctx, spawnID, "/root/.claude/skills", name, nonce, "", "")
}

// checkSkillTreesInImage runs `docker run --rm <imageRef> ...` (no live pod involved — this is
// exactly D1's "does the delta IMAGE capture both install trees intact" question) and reports
// presence + the skill dir's mode for each candidate root.
func checkSkillTreesInImage(ctx context.Context, imageRef, name string) (map[string]string, error) {
	script := fmt.Sprintf(
		`for d in /root/.agents/skills/%s /root/.claude/skills/%s; do if [ -d "$d" ]; then m=$(stat -c '%%a %%U' "$d" 2>/dev/null || echo "stat-failed"); c=$(cat "$d/SKILL.md" 2>/dev/null | tr -d '\n' || echo "read-failed"); echo "$d PRESENT mode=$m content=$c"; else echo "$d ABSENT"; fi; done`,
		name, name)
	// --entrypoint sh: the image's real ENTRYPOINT is deploy/agent/entrypoint.sh, which treats its
	// first arg as a runnable id (`exec launcher --runnable "${1:-}" ...`) — without overriding it,
	// `docker run <imageRef> sh -c ...` would be parsed as "launch the runnable named sh".
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--entrypoint", "sh", imageRef, "-c", script).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run --rm %s: %w (%s)", imageRef, err, strings.TrimSpace(string(out)))
	}
	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result, nil
}

func removeDockerImage(ctx context.Context, ref string) {
	_, _ = exec.CommandContext(ctx, "docker", "image", "rm", "-f", ref).CombinedOutput()
}

// TestSkillDeltaCaptureSpike answers §4.1's fork/suspend delta question:
//
//	D1 (image-level): does a rootfs delta image (via the FORK primitive, CaptureDeltaAs — NOT
//	CaptureDelta, which stops the source and would kill the pod mid-spike) capture both
//	~/.agents/skills and ~/.claude/skills intact, with the skill dir's mode preserved?
//
//	D2 (lifecycle-level): across a real Suspend()+recreate cycle (Manager-level, no CP/Garage
//	needed — Suspend's teardown captures a delta IFF ManagerConfig.DeltaCapture is true, and a
//	same-spawn-ID recreate's EnsureImage prefers that delta tag when present; this is the exact
//	mechanism production resume relies on, just without the CP orchestrating it), do the trees
//	survive with DeltaCapture=true vs DeltaCapture=false (the verified-in-code production default)?
func TestSkillDeltaCaptureSpike(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	report := loadSkillSpikeReport(t)

	// ---- D1: image-level, via CaptureDeltaAs (fork primitive; does NOT stop the source) --------
	t.Run("D1_image_level", func(t *testing.T) {
		// DeltaCapture on the manager config is irrelevant to CaptureDeltaAs — it's an unconditional
		// pod-backend call, not gated by cfg.DeltaCapture.
		h := newSkillSpikeHarness(t, false)
		defer h.cleanup()

		spawnID := "skillspike-delta-d1"
		targetID := "skillspike-delta-d1-target"
		sp := h.createProbePod(ctx, spawnID, "claude-tui", "tmux has-session -t spawn 2>/dev/null")

		nonce := skillSpikeRunID + "D1"
		h.plantBothSkillTrees(ctx, spawnID, "probe-delta", nonce)

		handle := &runtime.PodHandle{
			SpawnID:      sp.ID,
			AgentID:      sp.AgentID,
			SidecarID:    sp.SidecarID,
			BaseImageRef: sp.LaunchImageRef,
		}
		deltaRef, err := h.mgr.pod.CaptureDeltaAs(ctx, handle, targetID)
		if err != nil {
			t.Fatalf("CaptureDeltaAs: %v", err)
		}
		defer removeDockerImage(context.Background(), deltaRef)
		t.Logf("D1: captured delta %s (source spawn %s left running — CaptureDeltaAs does not stop it)", deltaRef, spawnID)

		// Prove the source is still alive (CaptureDeltaAs must not have stopped it) — a live
		// ExecStream against the ORIGINAL spawn is the check.
		sourceStillLive := true
		if _, _, code, execErr := h.exec(ctx, spawnID, "true"); execErr != nil || code != 0 {
			sourceStillLive = false
		}

		trees, err := checkSkillTreesInImage(ctx, deltaRef, "probe-delta")
		outcome := "ERROR"
		notes := ""
		if err != nil {
			notes = fmt.Sprintf("checkSkillTreesInImage failed: %v", err)
		} else {
			agentsLine := trees["/root/.agents/skills/probe-delta"]
			claudeLine := trees["/root/.claude/skills/probe-delta"]
			agentsOK := strings.HasPrefix(agentsLine, "PRESENT") && strings.Contains(agentsLine, nonce)
			claudeOK := strings.HasPrefix(claudeLine, "PRESENT") && strings.Contains(claudeLine, nonce)
			switch {
			case agentsOK && claudeOK:
				outcome = "BOTH_TREES_INTACT"
			case agentsOK != claudeOK:
				outcome = "ONE_TREE_ONLY"
			default:
				outcome = "NEITHER_TREE_PRESENT"
			}
			notes = fmt.Sprintf("source_still_live_after_capture=%v agents=%q claude=%q", sourceStillLive, agentsLine, claudeLine)
		}
		t.Logf("D1 outcome: %s (%s)", outcome, notes)
		report.Delta = append(report.Delta, skillDeltaResult{
			Kind: "D1-image-level (CaptureDeltaAs, fork primitive)", Outcome: outcome, Notes: notes,
		})
		writeSkillSpikeReport(t, report)
	})

	// ---- D2: lifecycle-level, Manager-only Suspend()+recreate ----------------------------------
	// Two arms, each its own Manager (a fresh DeltaCapture config value) and its own spawn ID (so
	// no leftover delta tag from one arm can leak into the other).
	for _, arm := range []struct {
		name         string
		deltaCapture bool
	}{
		{"DeltaCapture=true", true},
		{"DeltaCapture=false (PRODUCTION DEFAULT — verified in code, sp-mwco.2.1/2.2 ground truth)", false},
	} {
		arm := arm
		t.Run("D2_"+arm.name, func(t *testing.T) {
			h := newSkillSpikeHarness(t, arm.deltaCapture)
			defer h.cleanup()

			spawnID := fmt.Sprintf("skillspike-delta-d2-%v", arm.deltaCapture)
			h.createProbePod(ctx, spawnID, "claude-tui", "tmux has-session -t spawn 2>/dev/null")
			defer removeDockerImage(context.Background(), runtime.DeltaTag(spawnID))

			nonce := skillSpikeRunID + "D2"
			h.plantBothSkillTrees(ctx, spawnID, "probe-delta2", nonce)

			if _, err := h.mgr.Suspend(ctx, spawnID); err != nil {
				t.Fatalf("Suspend: %v", err)
			}
			t.Logf("D2 %s: suspended %s", arm.name, spawnID)

			// Recreate under the SAME spawn ID — EnsureImage prefers runtime.DeltaTag(spawnID) if
			// present locally (manager.go: deltaRef := runtime.DeltaTag(id); EnsureImage(baseRef,
			// deltaRef)), exactly the same-node-resume mechanism production Create uses.
			appDir := writeSkillSpikeApp(t)
			sp, err := h.mgr.CreateWithSelection(ctx, spawnID, appDir, skillSpikeModel, "", "", 1, AgentSelection{RunnableID: "claude-tui"})
			if err != nil {
				t.Fatalf("recreate after suspend: %v", err)
			}
			for _, w := range h.mgr.takeWatchers(sp) {
				w.Stop()
			}
			h.cleanups = append(h.cleanups, func() {
				cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, live := h.mgr.Store().Get(spawnID); live {
					_ = h.mgr.Stop(cctx, spawnID)
				}
			})
			relaunchedFromDelta := sp.LaunchImageRef == runtime.DeltaTag(spawnID)
			t.Logf("D2 %s: recreated, launch image = %s (from delta = %v)", arm.name, sp.LaunchImageRef, relaunchedFromDelta)

			agentsOut, _, _, _ := h.exec(ctx, spawnID, "cat /root/.agents/skills/probe-delta2/SKILL.md 2>&1 || echo ABSENT")
			claudeOut, _, _, _ := h.exec(ctx, spawnID, "cat /root/.claude/skills/probe-delta2/SKILL.md 2>&1 || echo ABSENT")
			agentsOK := strings.Contains(agentsOut, nonce)
			claudeOK := strings.Contains(claudeOut, nonce)

			outcome := "NEITHER_TREE_SURVIVED"
			switch {
			case agentsOK && claudeOK:
				outcome = "BOTH_TREES_SURVIVED"
			case agentsOK != claudeOK:
				outcome = "ONE_TREE_ONLY_SURVIVED"
			}
			notes := fmt.Sprintf("relaunched_from_delta_image=%v agents_present=%v claude_present=%v", relaunchedFromDelta, agentsOK, claudeOK)
			t.Logf("D2 %s outcome: %s (%s)", arm.name, outcome, notes)
			report.Delta = append(report.Delta, skillDeltaResult{
				Kind: "D2-lifecycle-" + arm.name, Outcome: outcome, Notes: notes,
			})
			writeSkillSpikeReport(t, report)
		})
	}
}
