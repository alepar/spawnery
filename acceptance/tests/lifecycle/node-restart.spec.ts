/**
 * SE3 acceptance: `systemctl restart spawnery-node` — the documented upgrade path — must NOT destroy the
 * node's running spawns (design 2026-07-12-spawnlet-restart-readoption-design.md §6).
 *
 *   create a spawn on the seeded github-slot app → write a marker file into its mount AND start a
 *   long-running process inside the agent → prove in-agent git-over-HTTPS works (baseline) →
 *   RESTART THE NODE (ACC_NODE_RESTART_CMD) → the spawn returns to ACTIVE with NO operator action →
 *   the marker file is still there, the long-running process is STILL ALIVE (same pid), and
 *   git-over-HTTPS still works (the per-spawn MITM CA survived the restart — sp-2tx8.3.5).
 *
 * TAG @noderestart: restarting the node disturbs every OTHER spawn on the box, so this scenario runs in its
 * OWN serial pass (scripts/e2e-vm/run.sh) and is excluded from the parallel pass.
 *
 * TARGET PRECONDITIONS:
 *   - ACC_NODE_RESTART_CMD — a host shell command that restarts the target's spawnlet (run.sh exports an
 *     `ssh … sudo systemctl restart spawnery-node` for the e2e-VM lane). Missing => this test FAILS (never
 *     skips); exclude it with `--grep-invert @noderestart` on targets whose node you cannot restart.
 *   - the seeded github-slot app (`spawnery/github-app`, ACC_GITHUB_APP_ID) and the Gitea lane, exactly as
 *     tests/mounts/git-persistence.spec.ts needs them.
 */

import { test, expect } from "../../src/harness/test";
import { execOrThrow, execInSpawn } from "../../src/scenarios/exec";
import { nodeAdminFromEnv, restartNode } from "../../src/scenarios/nodeadmin";
import { waitForStatus } from "../../src/scenarios/wait";

const appId = process.env.ACC_GITHUB_APP_ID ?? "spawnery/github-app";
const owner = process.env.ACC_GITHUB_OWNER ?? "spawnery";
const mountPath = "/app/repo";

function repoName(runId: string): string {
  const rand = Math.random().toString(36).slice(2, 8);
  return `acc-restart-${runId}-${rand}`.toLowerCase().replace(/[^a-z0-9._-]/g, "-");
}

test.describe("node restart", () => {
  // A restart is not a flake to be retried away: a retry would restart the node again and mask the very
  // regression this scenario exists to catch.
  test.describe.configure({ retries: 0 });

  test(
    "a spawnlet restart leaves the spawn running · @noderestart",
    { tag: ["@mutating", "@noderestart"] },
    async ({ ctx, cli, api, runId }) => {
      // create + clone-in + a full node restart + re-adoption is well past Playwright's 30s default.
      test.setTimeout(300_000);
      const admin = nodeAdminFromEnv(); // throws (loudly) if the lane cannot restart the node
      const cfg = cli.configuration();

      // 1. A live spawn with a github mount (an `origin` remote is what makes the git-over-HTTPS check real).
      const id = await cli.createSpawn(ctx, {
        appId,
        mounts: [{ name: "repo", backendUri: `github:${owner}/${repoName(runId)}`, create: true }],
      });
      await cli.waitActive(ctx, id);

      // 2. State that MUST survive: a marker file in the mount, and a long-running process in the agent.
      const marker = `restart-${runId}-${Date.now().toString(36)}`;
      await execOrThrow(cfg, ctx.identity, id, [
        "sh",
        "-c",
        `printf %s '${marker}' > ${mountPath}/acc-restart-marker.txt`,
      ]);
      const pid = (
        await execOrThrow(cfg, ctx.identity, id, ["sh", "-c", "nohup sleep 900 >/dev/null 2>&1 & echo $!"])
      ).stdout.trim();
      expect(pid, "the long-running process must report a pid").toMatch(/^\d+$/);
      await execOrThrow(cfg, ctx.identity, id, ["sh", "-c", `kill -0 ${pid}`]); // it really is running

      // 3. Baseline: in-agent git-over-HTTPS works (through the node's git proxy, trusting the per-spawn
      //    MITM CA). If THIS fails, the lane's git proxy is broken — not the restart.
      const before = await execInSpawn(cfg, ctx.identity, id, [
        "sh",
        "-c",
        `cd ${mountPath} && git ls-remote origin`,
      ]);
      expect(
        before.code,
        `precondition: git-over-HTTPS inside the agent does not work even BEFORE the restart: ${before.stderr}`,
      ).toBe(0);

      // 4. THE RESTART — the documented upgrade path.
      await restartNode(admin);

      // 5. The spawn comes back to ACTIVE on its own. No operator action, no MarkUnreachable that sticks.
      await waitForStatus(api, id, "ACTIVE", {
        timeoutMs: 120_000,
        timeoutHint:
          "the spawn did not return to ACTIVE after a node restart — re-adoption (SE3) either did not run " +
          "or the CP never re-delivered the launch spec.",
      });
      expect(await api.listSpawns()).toContainSpawn(id, { status: "ACTIVE" });

      // 6. The pod, its files and its in-flight work all survived.
      const restored = await execOrThrow(cfg, ctx.identity, id, ["cat", `${mountPath}/acc-restart-marker.txt`]);
      expect(restored.stdout.trim()).toBe(marker);
      const alive = await execInSpawn(cfg, ctx.identity, id, ["sh", "-c", `kill -0 ${pid}`]);
      expect(alive.code, `the long-running process (pid ${pid}) did not survive the restart`).toBe(0);
      const cmdline = await execOrThrow(cfg, ctx.identity, id, [
        "sh",
        "-c",
        `tr '\\0' ' ' < /proc/${pid}/cmdline`,
      ]);
      expect(cmdline.stdout, "pid was recycled by a different process").toContain("sleep");

      // 7. git-over-HTTPS still works: the agent's cached CA bundle is still the CA the node's proxy
      //    presents (sp-2tx8.3.5 — the CA is persisted, not regenerated on a cache miss).
      const after = await execInSpawn(cfg, ctx.identity, id, [
        "sh",
        "-c",
        `cd ${mountPath} && git ls-remote origin`,
      ]);
      expect(
        after.code,
        `git-over-HTTPS broke after the restart (the per-spawn MITM CA changed under the agent): ${after.stderr}`,
      ).toBe(0);
    },
  );
});
