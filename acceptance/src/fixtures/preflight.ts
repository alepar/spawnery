/**
 * Preflight health gate: before scenarios run, fail fast with a distinct "target/dependency
 * down" signal so an outage is not mistaken for a code regression (design §NFRs). The CP has no
 * /readyz or /version HTTP endpoint, so reachability is probed with a cheap, read-only,
 * owner-agnostic RPC (ListApps) plus a GET on the web origin.
 */

import type { AcceptanceClient } from "../drivers/oracle";
import type { TargetConfig } from "../config/target";

export class TargetDownError extends Error {
  constructor(reason: string) {
    super(`acceptance target unreachable: ${reason}`);
    this.name = "TargetDownError";
  }
}

export function classifyPreflight(result: { webOk: boolean; cpOk: boolean }): "ok" | "target-down" {
  return result.webOk && result.cpOk ? "ok" : "target-down";
}

/** runPreflight probes the target; throws TargetDownError (distinct from an assertion error) on failure. */
export async function runPreflight(cfg: TargetConfig, api: Pick<AcceptanceClient, "listApps">): Promise<void> {
  let webOk = false;
  try {
    const res = await fetch(cfg.webOrigin);
    webOk = res.status < 400;
  } catch (e) {
    throw new TargetDownError(`GET ${cfg.webOrigin} failed: ${(e as Error).message}`);
  }

  let cpOk = false;
  try {
    await api.listApps();
    cpOk = true;
  } catch (e) {
    throw new TargetDownError(`ListApps against ${cfg.cpEndpoint} failed: ${(e as Error).message}`);
  }

  if (classifyPreflight({ webOk, cpOk }) !== "ok") {
    throw new TargetDownError(`webOk=${webOk} cpOk=${cpOk}`);
  }
}
