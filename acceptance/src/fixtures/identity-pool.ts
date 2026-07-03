/**
 * Test-identity pool: a stable parallelIndex -> {owner,token} map, sized >= max Playwright
 * workers, never shared across workers (design §Isolation). dev-token mode supplies this
 * trivially from the same "token=owner,..." shape as the CP's CP_DEV_TOKENS.
 */

export interface Identity {
  token: string;
  owner: string;
}

/** parseIdentityPool parses the "token=owner,token2=owner2" format shared with CP_DEV_TOKENS. */
export function parseIdentityPool(spec: string): Identity[] {
  return spec
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
    .map((pair) => {
      const idx = pair.indexOf("=");
      if (idx < 0) throw new Error(`identity pool entry ${JSON.stringify(pair)} is not "token=owner"`);
      return { token: pair.slice(0, idx), owner: pair.slice(idx + 1) };
    });
}

/**
 * identityForWorker maps a Playwright parallelIndex to its dedicated identity. Throws if the
 * pool is too small — never silently shares an owner across workers.
 */
export function identityForWorker(pool: Identity[], parallelIndex: number): Identity {
  if (parallelIndex >= pool.length) {
    throw new Error(
      `identity pool has ${pool.length} entries but worker parallelIndex=${parallelIndex} needs one — ` +
        `grow ACC_IDENTITY_POOL or lower Playwright's "workers" setting`,
    );
  }
  return pool[parallelIndex];
}
