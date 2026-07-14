/**
 * sp-mwco.4.6 §4.8 S5 — the presign TTL (skillstore.PresignTTL = 30m) must cover the LAST by-ref
 * artifact's GET, not the first: every URL in an N-member bundle is minted together at StartSpawn,
 * so the artifact that finishes LAST is the one that determines whether the TTL held. This spec
 * measures StartSpawn -> ACTIVE (an upper bound on StartSpawn -> last-artifact-GET: ACTIVE also
 * waits out agent-install and health-check work after staging completes) across R iterations on a
 * K-member skill bundle, takes the max, and asserts headroom against the TTL.
 *
 * The exact StartSpawn -> last-GET figure (not just the ACTIVE upper bound) is emitted node-side as
 * `slog.Info("artifacts: by-ref staging complete", "count", K, "elapsed", d)` — see
 * internal/spawnlet/artifacts.go materializeByRef. Reading that log line requires shell/log access
 * to the node process, which the TS acceptance harness does not currently have a driver for (its
 * node-facing surface is `spawnctl exec` INSIDE the agent container, not the node's own stdout). In
 * the VM e2e lane (docs/e2e-vm-testing.md), that line is reachable via `vm_ssh <node-host>
 * 'journalctl -u spawnlet --since ...'` or the golden image's spawnlet log path — deliberately left
 * as a follow-up (a `scripts/e2e-vm/`-side grep, run alongside this spec, not a TS driver) rather
 * than building SSH into the acceptance harness for one measurement.
 *
 * Preconditions (fail loud, never skip):
 *   - ACC_SEED_SKILL_APP_ID: a registered app whose agent installs skills (see injection.spec.ts).
 *   - ACC_SKILL_SOURCE_REPOS: a comma-separated list of >= ACC_SKILL_BUNDLE_SIZE small public
 *     GitHub repos, each with a top-level (or `:subdir`-qualified) SKILL.md, e.g.
 *     "owner1/repo1,owner2/repo2:skills/foo,...". Real GitHub egress is required — skillfetch
 *     hardcodes api.github.com (sp-mwco.4.6 §4.6(b)), so this cannot run against a mocked/fake
 *     GitHub. Distinct repos are required because IngestSkillFromURL is idempotent on (creator,
 *     sha256 of content) — re-ingesting the same repo K times yields the SAME catalog id, not K.
 *
 * Run in the VM lane (cold image cache — a fresh golden-image VM has never pulled the agent image,
 * so CreateSpawn's image pull is NOT cached out of this measurement, matching a real self-hosted
 * node's first-ever spawn):
 *   GOLDEN_IMAGE=... scripts/e2e-vm/run.sh --profile fake --grep '@skill-staging'
 * (the golden image build is what makes the cache cold; nothing in this spec file manages that —
 * see docs/e2e-vm-testing.md). If the VM lane cannot reach GitHub for ingest, fall back to the
 * local dev stack (`just dev` + `just garage`) per the sp-mwco.4.6 plan; either way this spec
 * itself is lane-agnostic (it only needs target.cpEndpoint/nodeAddr and outbound GitHub access).
 */

import { test, expect } from "../../src/harness/test";
import { ProfileCli } from "../../src/drivers/customization";
import { SkillIngestOracle } from "../../src/drivers/skills";
import { assertDistinctCatalogIds, loadSkillStagingConfig } from "./skill-staging-s5-support";

// skillstore.PresignTTL (internal/cp/skillstore/skillstore.go:27). Not exposed to TS via any API;
// mirrored here as a literal — if the Go constant changes, this assertion's headroom must be
// re-derived by hand (there is no cross-language shared-constant mechanism in this repo).
const PRESIGN_TTL_MS = 30 * 60 * 1000;
// The plan's required headroom: max observed staging time must leave at least 2/3 of the TTL to
// spare (i.e. the measurement itself must be under 1/3 of the TTL).
const TTL_HEADROOM_DIVISOR = 3;

interface SourceRepo {
  url: string;
  subdir?: string;
}

function parseSourceRepos(raw: string | undefined): SourceRepo[] {
  if (!raw) return [];
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
    .map((entry) => {
      const [repo, subdir] = entry.split(":");
      return { url: `https://github.com/${repo}`, subdir: subdir || undefined };
    });
}

test(
  "staging S5: StartSpawn -> ACTIVE on the configured skill bundle stays well under the presign TTL",
  { tag: ["@mutating", "@skill-staging"] },
  async ({ target, identity, api, cli, ctx, ns }) => {
    const { bundleSize, iterations } = loadSkillStagingConfig();
    if (!target.seedSkillAppId) {
      throw new Error("ACC_SEED_SKILL_APP_ID is unset — see acceptance/.env.example (same precondition as injection.spec.ts).");
    }
    const repos = parseSourceRepos(process.env.ACC_SKILL_SOURCE_REPOS);
    if (repos.length < bundleSize) {
      throw new Error(
        `ACC_SKILL_SOURCE_REPOS provides ${repos.length} repo(s), need >= ${bundleSize} (ACC_SKILL_BUNDLE_SIZE). ` +
          `Set a comma-separated "owner/repo[:subdir]" list of small public GitHub repos each with a top-level SKILL.md.`,
      );
    }

    const oracle = new SkillIngestOracle(target.cpEndpoint, identity.token);
    const profileCli = new ProfileCli({ cpEndpoint: target.cpEndpoint, spawnctlBin: target.spawnctlBin }, identity);

    let profileId: string | undefined;
    const spawnIds: string[] = [];

    try {
      // --- ingest K distinct skills, then attach every one to a single profile ---
      profileId = await profileCli.create(ns("prof-s5"));
      const catalogIds: string[] = [];
      for (let i = 0; i < bundleSize; i++) {
        const repo = repos[i];
        const { catalogId } = await oracle.ingestSkillFromURL({
          url: repo.url,
          subdir: repo.subdir,
          name: ns(`s5-skill-${i}`),
        });
        catalogIds.push(catalogId);
      }
      assertDistinctCatalogIds(catalogIds, bundleSize);
      for (const [i, catalogId] of catalogIds.entries()) {
        await profileCli.entryAddCatalog(profileId, { kind: "skill", name: ns(`s5-entry-${i}`), catalogId });
      }

      const oracleProfile = await api.getProfile(profileId);
      expect(oracleProfile.entries).toHaveLength(bundleSize);

      // --- R iterations of StartSpawn -> ACTIVE, take the max ---
      const elapsedMs: number[] = [];
      for (let i = 0; i < iterations; i++) {
        const start = Date.now();
        const spawnId = await cli.createSpawn(ctx, { appId: target.seedSkillAppId, profileId });
        spawnIds.push(spawnId);
        await cli.waitActive(ctx, spawnId, { timeoutMs: 10 * 60 * 1000 });
        elapsedMs.push(Date.now() - start);

        const oracleSpawn = await api.findSpawn(spawnId);
        expect(oracleSpawn?.status).toBe("ACTIVE");
      }

      const maxElapsedMs = Math.max(...elapsedMs);
      const headroomMs = PRESIGN_TTL_MS / TTL_HEADROOM_DIVISOR;
      // This IS the §4.8 measurement; it must be visible in CI output.
      console.log(
        `S5: StartSpawn -> ACTIVE elapsed over ${iterations} iteration(s) on a ${bundleSize}-member bundle: ` +
          `[${elapsedMs.join(", ")}] ms, max=${maxElapsedMs}ms (presign TTL=${PRESIGN_TTL_MS}ms, headroom budget=${headroomMs}ms)`,
      );

      expect(
        maxElapsedMs,
        `max StartSpawn->ACTIVE (${maxElapsedMs}ms) must stay under presign_ttl/${TTL_HEADROOM_DIVISOR} (${headroomMs}ms) ` +
          `for a ${bundleSize}-member bundle`,
      ).toBeLessThan(headroomMs);
    } finally {
      for (const id of spawnIds) await api.deleteSpawn(id).catch(() => {});
      if (profileId) await profileCli.delete(profileId).catch(() => {});
    }
  },
);
