/**
 * Prod-safety guardrail: tag-based default-deny. A test is treated as MUTATING unless it is
 * explicitly tagged @readonly; a mutating test hard-fails unless targetHost is on the non-prod
 * allowlist (design §Isolation, prod-safety guardrail — default-deny, derived from the actual
 * target URL, never a self-declared label).
 */

import { isNonProd } from "../config/target";

export class ProdGuardrailError extends Error {
  constructor(targetHost: string) {
    super(
      `prod-safety guardrail: mutating test blocked against ${JSON.stringify(targetHost)} — ` +
        `it is not on the non-prod allowlist. Tag the test @readonly if it truly makes no writes, ` +
        `or point ACC_WEB_ORIGIN at a non-prod target.`,
    );
    this.name = "ProdGuardrailError";
  }
}

/** isReadonly reports whether a test's tag set marks it explicitly @readonly. */
export function isReadonlyTags(testTags: string[]): boolean {
  return testTags.includes("@readonly");
}

/**
 * guardMutation throws ProdGuardrailError when a mutating (i.e. not explicitly @readonly) test
 * is about to run against a host that isn't on the non-prod allowlist. Untagged tests are
 * treated as mutating — default-deny.
 */
export function guardMutation(testTags: string[], targetHost: string, nonprodHosts: string[]): void {
  if (isReadonlyTags(testTags)) return;
  if (isNonProd(targetHost, nonprodHosts)) return;
  throw new ProdGuardrailError(targetHost);
}
