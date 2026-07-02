/**
 * Version-pin comparator: the built spawnctl/oracle are pinned to the target's expected ref
 * (config-declared — the CP has no live /version endpoint yet, see the epic's follow-up beads).
 * A mismatch throws a distinct VersionSkewError so a contract-skew failure isn't misread as a
 * code regression.
 */

export class VersionSkewError extends Error {
  constructor(expectedRef: string, actualRef: string) {
    super(`version skew: target expects ref ${JSON.stringify(expectedRef)}, this run was built from ${JSON.stringify(actualRef)}`);
    this.name = "VersionSkewError";
  }
}

export function assertVersionPin(expectedRef: string, actualRef: string): void {
  if (expectedRef !== actualRef) throw new VersionSkewError(expectedRef, actualRef);
}
