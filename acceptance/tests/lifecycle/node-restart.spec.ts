/**
 * SE3 acceptance: `systemctl restart spawnery-node` — the documented upgrade path — must NOT destroy the
 * node's running spawns (design 2026-07-12-spawnlet-restart-readoption-design.md §6).
 *
 *   create a spawn on the seeded github-slot app → write a marker file into its mount AND start a
 *   tmux-supervised long-running process inside the agent → prove in-agent git-over-HTTPS works (baseline) →
 *   RESTART THE NODE (ACC_NODE_RESTART_CMD) → the spawn returns to ACTIVE with NO operator action →
 *   the marker file is still there, the same pane ID/PID/start time/cmdline is STILL ALIVE, and
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
import { execOrThrow, execInSpawn, type ExecConfig } from "../../src/scenarios/exec";
import { nodeAdminFromEnv, restartNode } from "../../src/scenarios/nodeadmin";
import { waitForStatus } from "../../src/scenarios/wait";
import type { Identity } from "../../src/fixtures/identity-pool";

const appId = process.env.ACC_GITHUB_APP_ID ?? "spawnery/github-app";
const owner = process.env.ACC_GITHUB_OWNER ?? "spawnery";
const mountPath = "/app/repo";
const probeCmdline = "sleep 900 ";

interface PaneProcessIdentity {
  pid: string;
  startTime: string;
  cmdline: string;
}

async function waitForPaneProcess(
  cfg: ExecConfig,
  identity: Identity,
  spawnId: string,
  paneId: string,
  timeoutMs = 10_000,
): Promise<PaneProcessIdentity> {
  const deadline = Date.now() + timeoutMs;
  let lastPid = "unknown";
  let lastCmdline = "unreadable";
  let lastError = "none";

  for (;;) {
    const pane = await execInSpawn(cfg, identity, spawnId, [
      "tmux", "display-message", "-p", "-t", paneId, "#{pane_pid}",
    ]);
    const pid = pane.stdout.trim();
    if (pane.code === 0 && /^\d+$/.test(pid)) {
      lastPid = pid;
      const cmdline = await execInSpawn(cfg, identity, spawnId, [
        "sh", "-c", `tr '\\0' ' ' < /proc/${pid}/cmdline`,
      ]);
      lastCmdline = cmdline.stdout;
      lastError = cmdline.code === 0 ? "none" : cmdline.stderr.trim() || `cmdline exited ${cmdline.code}`;

      if (cmdline.code === 0 && cmdline.stdout === probeCmdline) {
        const startTime = await execInSpawn(cfg, identity, spawnId, [
          "sh", "-c", `awk '{print $22}' /proc/${pid}/stat`,
        ]);
        const confirmedPane = await execInSpawn(cfg, identity, spawnId, [
          "tmux", "display-message", "-p", "-t", paneId, "#{pane_pid}",
        ]);
        if (
          startTime.code === 0 && /^\d+$/.test(startTime.stdout.trim()) &&
          confirmedPane.code === 0 && confirmedPane.stdout.trim() === pid
        ) {
          return { pid, startTime: startTime.stdout.trim(), cmdline: cmdline.stdout };
        }
        lastError = startTime.stderr.trim() || confirmedPane.stderr.trim() || "pane PID changed during observation";
      }
    } else {
      lastError = pane.stderr.trim() || `tmux display-message exited ${pane.code}`;
    }

    if (Date.now() >= deadline) {
      throw new Error(
        `tmux pane ${paneId} did not exec ${JSON.stringify(probeCmdline)} within ${timeoutMs}ms ` +
          `(last pid ${lastPid}, cmdline ${JSON.stringify(lastCmdline)}, error ${JSON.stringify(lastError)})`,
      );
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
}

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
      const id = await api.createSpawn({
        appId,
        model: process.env.ACC_TEST_MODEL ?? "openai/gpt-4o-mini",
        name: ctx.ns("restart"),
        image: "spawnery/agent:dev",
        runnableId: "opencode-tui",
        mounts: [
          {
            name: "repo",
            backendUri: `github:${owner}/${repoName(runId)}`,
            createIfMissing: true,
          },
        ],
      });
      await waitForStatus(api, id, "ACTIVE");

      // 2. State that MUST survive: a marker file in the mount, and a tmux-supervised process in the agent.
      const marker = `restart-${runId}-${Date.now().toString(36)}`;
      await execOrThrow(cfg, ctx.identity, id, [
        "sh",
        "-c",
        `printf %s '${marker}' > ${mountPath}/acc-restart-marker.txt`,
      ]);
      await execOrThrow(cfg, ctx.identity, id, ["tmux", "has-session", "-t", "spawn"]);

      const probeWindow = marker.toLowerCase().replace(/[^a-z0-9._-]/g, "-");
      const paneId = (
        await execOrThrow(cfg, ctx.identity, id, [
          "tmux",
          "new-window",
          "-d",
          "-P",
          "-F",
          "#{pane_id}",
          "-t",
          "spawn",
          "-n",
          probeWindow,
          "--",
          "sleep",
          "900",
        ])
      ).stdout.trim();
      expect(paneId, "the tmux probe window must report a pane ID").toMatch(/^%\d+$/);

      const { pid, startTime, cmdline: processCmdline } = await waitForPaneProcess(
        cfg,
        ctx.identity,
        id,
        paneId,
      );
      await execOrThrow(cfg, ctx.identity, id, ["sh", "-c", `kill -0 ${pid}`]);
      expect(processCmdline, "the tmux pane must run only the durable probe").toBe(probeCmdline);

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
      await execOrThrow(cfg, ctx.identity, id, ["tmux", "has-session", "-t", "spawn"]);
      const restoredPaneId = (
        await execOrThrow(cfg, ctx.identity, id, [
          "tmux",
          "display-message",
          "-p",
          "-t",
          paneId,
          "#{pane_id}",
        ])
      ).stdout.trim();
      expect(restoredPaneId, "the tmux pane ID changed across restart").toBe(paneId);

      const restoredPid = (
        await execOrThrow(cfg, ctx.identity, id, [
          "tmux",
          "display-message",
          "-p",
          "-t",
          paneId,
          "#{pane_pid}",
        ])
      ).stdout.trim();
      expect(restoredPid, "the tmux pane PID changed across restart").toBe(pid);

      const alive = await execInSpawn(cfg, ctx.identity, id, ["sh", "-c", `kill -0 ${restoredPid}`]);
      expect(alive.code, `the tmux pane process (pid ${restoredPid}) did not survive the restart`).toBe(0);

      const restoredStartTime = (
        await execOrThrow(cfg, ctx.identity, id, [
          "sh",
          "-c",
          `awk '{print $22}' /proc/${restoredPid}/stat`,
        ])
      ).stdout.trim();
      expect(restoredStartTime, "the tmux pane PID was recycled across restart").toBe(startTime);

      const restoredCmdline = (
        await execOrThrow(cfg, ctx.identity, id, [
          "sh",
          "-c",
          `tr '\\0' ' ' < /proc/${restoredPid}/cmdline`,
        ])
      ).stdout;
      expect(restoredCmdline, "the tmux pane command changed across restart").toBe(processCmdline);

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
