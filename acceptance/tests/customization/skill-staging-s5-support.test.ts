import { describe, expect, it } from "vitest";
import { assertDistinctCatalogIds, loadSkillStagingConfig } from "./skill-staging-s5-support";

describe("loadSkillStagingConfig", () => {
  it("uses the S5 defaults when tuning variables are unset", () => {
    expect(loadSkillStagingConfig({})).toEqual({ bundleSize: 8, iterations: 5 });
  });

  it("accepts finite integer overrides within the S5 bounds", () => {
    expect(loadSkillStagingConfig({
      ACC_SKILL_BUNDLE_SIZE: "12",
      ACC_SKILL_STAGING_ITERATIONS: "3",
    })).toEqual({ bundleSize: 12, iterations: 3 });
  });

  it.each(["NaN", "Infinity", "8.5", "7", "0", "-1"])(
    "rejects invalid bundle size %s",
    (bundleSize) => {
      expect(() => loadSkillStagingConfig({ ACC_SKILL_BUNDLE_SIZE: bundleSize })).toThrow(
        `ACC_SKILL_BUNDLE_SIZE must be a finite integer >= 8; got ${JSON.stringify(bundleSize)}`,
      );
    },
  );

  it.each(["NaN", "Infinity", "1.5", "0", "-1"])(
    "rejects invalid iteration count %s",
    (iterations) => {
      expect(() => loadSkillStagingConfig({ ACC_SKILL_STAGING_ITERATIONS: iterations })).toThrow(
        `ACC_SKILL_STAGING_ITERATIONS must be a finite integer >= 1; got ${JSON.stringify(iterations)}`,
      );
    },
  );
});

describe("assertDistinctCatalogIds", () => {
  it("accepts a bundle with the required number of distinct catalog IDs", () => {
    expect(() => assertDistinctCatalogIds(
      Array.from({ length: 8 }, (_, index) => `catalog-${index}`),
      8,
    )).not.toThrow();
  });

  it("rejects an apparently full bundle whose sources deduplicate to fewer catalog IDs", () => {
    expect(() => assertDistinctCatalogIds([
      "catalog-0",
      "catalog-1",
      "catalog-2",
      "catalog-3",
      "catalog-4",
      "catalog-5",
      "catalog-6",
      "catalog-6",
    ], 8)).toThrow(
      "S5 requires at least 8 distinct catalog IDs after ingest; got 7 from 8 ingest result(s)",
    );
  });
});
