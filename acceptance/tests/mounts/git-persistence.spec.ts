/**
 * github: storage mount durability — a `github:` mount's committed contents survive suspend→resume.
 *
 * Proves the real durability semantic of a github slot (design §16.7): the initial clone + the
 * agent's LOCAL (unpushed) commit are captured by the node-local Kopia journal on suspend and
 * restored on resume — NOT via a suspend-time remote push (the backstop is deferred, sp-u53.8). So:
 *
 *   create a spawn on the seeded github-slot app, binding the slot to a fresh repo (clone-in) →
 *   write + `git commit` a per-run marker INTO the mount via `spawnctl exec` (local, not pushed) →
 *   suspend (journal snapshots .git) → resume (journal restore, clone skipped) →
 *   assert the committed marker file AND the exact commit hash are present in the resumed spawn.
 *
 * assertFreshMarker-style freshness: the marker is unique to THIS run, so a stale file left by a
 * prior run cannot satisfy the assertion (mirrors marker.ts / suspend-fork.spec.ts).
 *
 * TARGET PRECONDITIONS (the e2e-vm Gitea lane, scripts/e2e-vm/provision):
 *   - a seeded app with a github slot — `spawnery/github-app` (Ref examples/github-app), overridable
 *     via ACC_GITHUB_APP_ID;
 *   - the node runs with the github-static-token override (GITHUB_API_BASE_URL / GITHUB_HOST /
 *     GITHUB_ALLOW_INSECURE_HOST / GITHUB_STATIC_TOKEN) pointed at the VM's local Gitea; and
 *   - ACC_GITHUB_OWNER is the Gitea account that owns created repos (the token's user; default
 *     "spawnery", matching the provision bootstrap).
 * As with suspend-fork's Garage dependency, this test FAILS (never skips) if the lane is absent.
 */

import { test, expect } from "../../src/harness/test";
import { execConfigFromTarget, execOrThrow } from "../../src/scenarios/exec";
import { waitForStatus, GARAGE_HINT } from "../../src/scenarios/wait";

// The seeded app exposing a github SLOT mount named "repo" at path "repo" (examples/github-app).
const appId = process.env.ACC_GITHUB_APP_ID ?? "spawnery/github-app";
// The Gitea account that owns created repos (the static token's user). Provision default: "spawnery".
const owner = process.env.ACC_GITHUB_OWNER ?? "spawnery";
// examples/github-app declares the mount at path "repo" → bind-mounted at /app/repo (manager.go).
const mountPath = "/app/repo";

/** giteaRepoName: a Gitea-safe, per-run-unique repo name so parallel/repeated runs never collide. */
function giteaRepoName(runId: string): string {
  const rand = Math.random().toString(36).slice(2, 8);
  return `acc-gitmount-${runId}-${rand}`.toLowerCase().replace(/[^a-z0-9._-]/g, "-");
}

test("github mount survives suspend/resume · cli", { tag: "@mutating" }, async ({ ctx, cli, api, runId, target }) => {
  const cfg = execConfigFromTarget(target);

  // 1. Create a spawn on the seeded github-slot app, binding the slot to a fresh Gitea repo
  //    (create_if_missing → clone-in of an empty repo; the CP derives the gh: credential, the node's
  //    static token backs the clone).
  const repo = giteaRepoName(runId);
  const id = await cli.createSpawn(ctx, {
    appId,
    mounts: [{ name: "repo", backendUri: `github:${owner}/${repo}`, create: true }],
  });
  await cli.waitActive(ctx, id);

  // 2. Write a per-run marker into the mount and COMMIT it locally (not pushed) — the normal
  //    in-spawn git workflow. Identity is passed inline so the commit needs no seeded gitconfig.
  const marker = `gitmount-${runId}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
  const gitId = "-c user.name=acc -c user.email=acc@e2e.test";
  await execOrThrow(cfg, id, [
    "sh",
    "-c",
    `set -e; cd ${mountPath}; printf %s '${marker}' > acc-git-marker.txt; ` +
      `git ${gitId} add acc-git-marker.txt; git ${gitId} commit -m 'acc marker'`,
  ]);
  const head = (await execOrThrow(cfg, id, ["sh", "-c", `cd ${mountPath} && git rev-parse HEAD`])).stdout.trim();

  // 3. Suspend (workspace → journal, pod torn down) then resume (journal → new pod). The Garage
  //    journal is the durability substrate — GARAGE_HINT frames a timeout as that dependency.
  await cli.suspend(ctx, id);
  await waitForStatus(api, id, "SUSPENDED", { timeoutHint: GARAGE_HINT });
  await cli.resume(ctx, id);
  await cli.waitActive(ctx, id);
  expect(await api.listSpawns()).toContainSpawn(id, { status: "ACTIVE" });

  // 4. The committed marker file AND the exact commit survived the journal round-trip (restored,
  //    not re-cloned — the unpushed commit only exists in the journal-captured .git).
  const restored = await execOrThrow(cfg, id, ["cat", `${mountPath}/acc-git-marker.txt`]);
  expect(restored.stdout.trim()).toBe(marker);
  const restoredHead = (await execOrThrow(cfg, id, ["sh", "-c", `cd ${mountPath} && git rev-parse HEAD`])).stdout.trim();
  expect(restoredHead).toBe(head);
});
