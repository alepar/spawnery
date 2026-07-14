export interface SkillStagingConfig {
  bundleSize: number;
  iterations: number;
}

function finiteIntegerAtLeast(
  env: NodeJS.ProcessEnv,
  name: "ACC_SKILL_BUNDLE_SIZE" | "ACC_SKILL_STAGING_ITERATIONS",
  defaultValue: number,
  minimum: number,
): number {
  const raw = env[name] ?? String(defaultValue);
  const value = Number(raw);
  if (!Number.isFinite(value) || !Number.isInteger(value) || value < minimum) {
    throw new Error(`${name} must be a finite integer >= ${minimum}; got ${JSON.stringify(raw)}`);
  }
  return value;
}

export function loadSkillStagingConfig(env: NodeJS.ProcessEnv = process.env): SkillStagingConfig {
  return {
    bundleSize: finiteIntegerAtLeast(env, "ACC_SKILL_BUNDLE_SIZE", 8, 8),
    iterations: finiteIntegerAtLeast(env, "ACC_SKILL_STAGING_ITERATIONS", 5, 1),
  };
}

export function assertDistinctCatalogIds(catalogIds: readonly string[], bundleSize: number): void {
  const distinctCount = new Set(catalogIds).size;
  if (distinctCount < bundleSize) {
    throw new Error(
      `S5 requires at least ${bundleSize} distinct catalog IDs after ingest; ` +
        `got ${distinctCount} from ${catalogIds.length} ingest result(s)`,
    );
  }
}
