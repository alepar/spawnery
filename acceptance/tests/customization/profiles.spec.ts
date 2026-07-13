/**
 * Phase 5 — customization profiles: CRUD + entries + secret-refs, cli-primary (spawnctl `profile`)
 * with the apiDriver oracle as the cross-check (design §API oracle). Profiles/entries/secret-refs
 * are NOT covered by the spawn teardown sweeper (fixtures/sweep.ts only sweeps spawns), so this
 * scenario deletes everything it creates explicitly, in a finally block.
 */

import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test, expect } from "../../src/harness/test";
import { ProfileCli, buildSkillTar, dummyAtRestEnvelope } from "../../src/drivers/customization";

test(
  "profiles: create -> list -> show -> rename -> entry add/remove -> secret-ref add/remove -> delete",
  { tag: "@mutating" },
  async ({ identity, auth, api, cli, ns }) => {
    const profileCli = new ProfileCli(cli.configuration(), identity);
    const accountId = await auth.accountId(identity);
    const name = ns("prof-crud");

    let profileId: string | undefined;
    let secretId: string | undefined;
    let tmpDir: string | undefined;

    try {
      // --- create ---
      profileId = await profileCli.create(name);
      expect(profileId.length).toBeGreaterThan(0);
      let oracle = await api.getProfile(profileId);
      expect(oracle.name).toBe(name);
      expect(oracle.version).toBe(1);
      expect(oracle.entries).toEqual([]);
      expect(oracle.secretIds).toEqual([]);

      // --- list ---
      const listOut = await profileCli.list();
      expect(listOut).toContain(profileId);
      const oracleList = await api.listProfiles();
      expect(oracleList.some((p) => p.profileId === profileId)).toBe(true);

      // --- show ---
      const showOut = await profileCli.show(profileId);
      expect(showOut).toContain(profileId);
      expect(showOut).toContain(name);

      // --- rename ---
      const renamedName = `${name}-renamed`;
      await profileCli.rename(profileId, renamedName);
      oracle = await api.getProfile(profileId);
      expect(oracle.name).toBe(renamedName);
      expect(oracle.version).toBeGreaterThan(1);
      const versionAfterRename = oracle.version;

      // --- entry add (custom skill, built via buildSkillTar and written to a tmp file) ---
      tmpDir = mkdtempSync(join(tmpdir(), "acc-profile-"));
      const tarPath = join(tmpDir, "skill.tar");
      writeFileSync(tarPath, buildSkillTar(`MARKER=${ns("entry")}`));
      const entryName = ns("entry-skill");
      const entryId = await profileCli.entryAddCustom(profileId, { kind: "skill", name: entryName, customFilePath: tarPath });
      expect(entryId.length).toBeGreaterThan(0);

      oracle = await api.getProfile(profileId);
      expect(oracle.entries).toHaveLength(1);
      expect(oracle.entries[0]).toMatchObject({ entryId, kind: "PROFILE_ENTRY_KIND_SKILL", name: entryName, source: "PROFILE_ENTRY_SOURCE_CUSTOM" });
      expect(oracle.version).toBeGreaterThan(versionAfterRename);

      const showAfterEntry = await profileCli.show(profileId);
      expect(showAfterEntry).toContain(entryId);
      expect(showAfterEntry).toContain(entryName);

      // --- entry remove ---
      const versionAfterEntry = oracle.version;
      await profileCli.entryRemove(profileId, entryId);
      oracle = await api.getProfile(profileId);
      expect(oracle.entries).toEqual([]);
      expect(oracle.version).toBeGreaterThan(versionAfterEntry);

      // --- secret-ref add/remove ---
      // Secret CRUD is oracle-only (no `spawnctl secret` subcommand — CLI parity gap, sp-tq0t);
      // see secrets.spec.ts. Here we only exercise the profile<->secret attach/detach verbs, which
      // ARE cli-primary (`spawnctl profile secret add|remove`).
      secretId = ns("sec-profref");
      await api.createSecret({
        secretId,
        type: "USER_SECRET_TYPE_GENERIC_KV",
        name: secretId,
        targetContainer: "ARTIFACT_TARGET_AGENT",
        envVarName: "ACC_SECRET_PROFREF",
        devicesetEpoch: 0,
        envelope: dummyAtRestEnvelope(accountId, secretId),
      });

      const versionAfterEntryRemove = oracle.version;
      await profileCli.secretAdd(profileId, secretId);
      oracle = await api.getProfile(profileId);
      expect(oracle.secretIds).toContain(secretId);
      expect(oracle.version).toBeGreaterThan(versionAfterEntryRemove);

      const versionAfterSecretAdd = oracle.version;
      await profileCli.secretRemove(profileId, secretId);
      oracle = await api.getProfile(profileId);
      expect(oracle.secretIds).not.toContain(secretId);
      expect(oracle.version).toBeGreaterThan(versionAfterSecretAdd);

      // --- delete ---
      await profileCli.delete(profileId);
      await expect(api.getProfile(profileId)).rejects.toThrow();
      const deletedId = profileId;
      profileId = undefined; // deleted; the finally block below must not try again

      const oracleListAfterDelete = await api.listProfiles();
      expect(oracleListAfterDelete.some((p) => p.profileId === deletedId)).toBe(false);
    } finally {
      if (profileId) await profileCli.delete(profileId).catch(() => {});
      if (secretId) await api.deleteSecret(secretId).catch(() => {});
      if (tmpDir) rmSync(tmpDir, { recursive: true, force: true });
    }
  },
);
