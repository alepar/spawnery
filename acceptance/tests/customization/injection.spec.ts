/**
 * Phase 5 — profile-attach injection: attach a profile (custom skill entry) at spawn-create,
 * observe materialization inside the running spawn via authenticated, CP-relayed `spawnctl exec`.
 *
 * Preconditions (fail loud, never skip — design's "every dep-gated scenario fails loud, not a
 * skip"): a registered seed app whose agent installs skills
 * (target.seedSkillAppId; claude installs to `.claude/skills`, codex to `.codex/skills` —
 * internal/agentinstall/{claude,codex}.go). Both are documented in .env.example.
 *
 * Secret injection is explicitly out of scope (see secrets.spec.ts) — this observes a non-secret
 * skill entry instead, asserting a FRESH per-run marker (never mere file existence, which would
 * pass on a stale artifact from a prior run).
 */

import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test, expect } from "../../src/harness/test";
import { ProfileCli, execInSpawn, buildSkillTar } from "../../src/drivers/customization";

test(
  "injection: a profile-attached custom skill entry materializes in the spawn (observed via spawnctl exec)",
  { tag: "@mutating" },
  async ({ target, identity, api, cli, ctx, ns }) => {
    if (!target.seedSkillAppId) {
      throw new Error(
        "ACC_SEED_SKILL_APP_ID is unset — injection.spec.ts needs a registered app whose agent installs skills " +
          "(claude or codex); see acceptance/.env.example.",
      );
    }
    const model = process.env.ACC_TEST_MODEL;
    if (!model) throw new Error("ACC_TEST_MODEL is unset — injection.spec.ts needs a model for the selected agent runnable");

    const profileCli = new ProfileCli(cli.configuration(), identity);
    const cliCfg = cli.configuration();

    let profileId: string | undefined;
    let spawnId: string | undefined;
    let tmpDir: string | undefined;

    try {
      profileId = await profileCli.create(ns("prof-inject"));

      const entryName = ns("accskill");
      const marker = `MARKER=${ns("inject")}-${Date.now()}`;
      tmpDir = mkdtempSync(join(tmpdir(), "acc-injection-"));
      const tarPath = join(tmpDir, "skill.tar");
      writeFileSync(tarPath, buildSkillTar(marker));
      await profileCli.entryAddCustom(profileId, { kind: "skill", name: entryName, customFilePath: tarPath });

      // spawnctl create cannot select an image/runnable. Use the production SDK oracle for the
      // canonical agentinstall lane, then observe the result through spawnctl below.
      spawnId = await api.createSpawn({
        appId: target.seedSkillAppId,
        model,
        profileId,
        image: "spawnery/agent:dev",
        runnableId: "claude-tui",
      });
      await cli.waitActive(ctx, spawnId);

      // Cross-check: the spawn is ACTIVE via the oracle too.
      const oracleSpawn = await api.findSpawn(spawnId);
      expect(oracleSpawn?.status).toBe("ACTIVE");

      // Observe materialization: agent-agnostic via the `*skills/*` glob (claude: .claude/skills,
      // codex: .codex/skills — internal/agentinstall/{claude,codex}.go).
      const result = await execInSpawn(cliCfg, identity, spawnId, [
        "sh",
        "-lc",
        `find "$HOME" -path "*skills/${entryName}/SKILL.md" -exec cat {} +`,
      ]);

      expect(result.code, `exec exited ${result.code}; stderr:\n${result.stderr}\nstdout:\n${result.stdout}`).toBe(0);
      expect(result.stdout).toContain(marker);
    } finally {
      if (spawnId) await api.deleteSpawn(spawnId).catch(() => {});
      if (profileId) await profileCli.delete(profileId).catch(() => {});
      if (tmpDir) rmSync(tmpDir, { recursive: true, force: true });
    }
  },
);
