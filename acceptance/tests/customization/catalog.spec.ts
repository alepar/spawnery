/**
 * Phase 5 — curated catalog entries: CRUD + listing toggle, cli-primary (spawnctl `catalog`) with
 * the apiDriver oracle as the cross-check. Also covers the catalog-ref profile-entry path (a
 * profile entry sourced from a catalog entry rather than inline custom content). Neither catalog
 * entries nor profiles are covered by the spawn teardown sweeper — cleaned up explicitly here.
 */

import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test, expect } from "../../src/harness/test";
import { CatalogCli, ProfileCli, buildSkillTar } from "../../src/drivers/customization";

test(
  "catalog: create -> list -> show -> update -> set-listing -> delete",
  { tag: "@mutating" },
  async ({ identity, api, cli, ns }) => {
    const catalogCli = new CatalogCli(cli.configuration(), identity);
    const name = ns("cat-crud");

    let catalogId: string | undefined;
    let tmpDir: string | undefined;

    try {
      // --- create ---
      tmpDir = mkdtempSync(join(tmpdir(), "acc-catalog-"));
      const tarPath = join(tmpDir, "skill.tar");
      writeFileSync(tarPath, buildSkillTar(`MARKER=${ns("catalog-content")}`));
      catalogId = await catalogCli.create({ name, kind: "skill", description: "acceptance catalog entry", contentFilePath: tarPath });
      expect(catalogId.length).toBeGreaterThan(0);

      let oracle = await api.getCatalogEntry(catalogId);
      expect(oracle.name).toBe(name);
      expect(oracle.kind).toBe("PROFILE_ENTRY_KIND_SKILL");
      expect(oracle.description).toBe("acceptance catalog entry");
      expect(oracle.listed).toBe(false); // not listed until set-listing

      // --- list ---
      const listOut = await catalogCli.list();
      expect(listOut).toContain(catalogId);
      const oracleList = await api.listCatalogEntries();
      expect(oracleList.some((e) => e.catalogId === catalogId)).toBe(true);

      // --- show ---
      const showOut = await catalogCli.show(catalogId);
      expect(showOut).toContain(catalogId);
      expect(showOut).toContain(name);

      // --- update ---
      const newDescription = "updated acceptance catalog entry";
      await catalogCli.update(catalogId, { description: newDescription });
      oracle = await api.getCatalogEntry(catalogId);
      expect(oracle.description).toBe(newDescription);

      // --- set-listing ---
      await catalogCli.setListing(catalogId, true);
      oracle = await api.getCatalogEntry(catalogId);
      expect(oracle.listed).toBe(true);

      await catalogCli.setListing(catalogId, false);
      oracle = await api.getCatalogEntry(catalogId);
      expect(oracle.listed).toBe(false);

      // --- delete ---
      await catalogCli.delete(catalogId);
      await expect(api.getCatalogEntry(catalogId)).rejects.toThrow();
      const deletedId = catalogId;
      catalogId = undefined; // deleted; the finally block below must not try again

      const oracleListAfterDelete = await api.listCatalogEntries();
      expect(oracleListAfterDelete.some((e) => e.catalogId === deletedId)).toBe(false);
    } finally {
      if (catalogId) await catalogCli.delete(catalogId).catch(() => {});
      if (tmpDir) rmSync(tmpDir, { recursive: true, force: true });
    }
  },
);

test(
  "catalog: a profile entry can source from a catalog entry (catalog-ref)",
  { tag: "@mutating" },
  async ({ identity, api, cli, ns }) => {
    const catalogCli = new CatalogCli(cli.configuration(), identity);
    const profileCli = new ProfileCli(cli.configuration(), identity);

    let catalogId: string | undefined;
    let profileId: string | undefined;
    let tmpDir: string | undefined;

    try {
      tmpDir = mkdtempSync(join(tmpdir(), "acc-catalog-ref-"));
      const tarPath = join(tmpDir, "skill.tar");
      writeFileSync(tarPath, buildSkillTar(`MARKER=${ns("catalog-ref-content")}`));
      const catalogName = ns("cat-ref");
      catalogId = await catalogCli.create({ name: catalogName, kind: "skill", contentFilePath: tarPath });

      profileId = await profileCli.create(ns("prof-catalog-ref"));
      const entryName = ns("entry-catalog-ref");
      const entryId = await profileCli.entryAddCatalog(profileId, { kind: "skill", name: entryName, catalogId });
      expect(entryId.length).toBeGreaterThan(0);

      const oracle = await api.getProfile(profileId);
      expect(oracle.entries).toHaveLength(1);
      expect(oracle.entries[0]).toMatchObject({
        entryId,
        name: entryName,
        source: "PROFILE_ENTRY_SOURCE_CATALOG_REF",
        catalogId,
      });
    } finally {
      if (profileId) await profileCli.delete(profileId).catch(() => {});
      if (catalogId) await catalogCli.delete(catalogId).catch(() => {});
      if (tmpDir) rmSync(tmpDir, { recursive: true, force: true });
    }
  },
);
