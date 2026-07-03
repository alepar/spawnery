/**
 * SpawnDriver: the common interface both surface drivers (web, cli) implement, per the design's
 * driver model. cliDriver stubs the verbs spawnctl cannot perform (rename/suspend/stop/delete) as
 * FAILING calls — never a skip — surfacing the CLI parity gap as visible red (design §Coverage).
 */

import type { ApiDriver } from "./api";
import type { Identity } from "../fixtures/identity-pool";

export type SpawnId = string;

// Mirrors the wire enum's serialized string form (cp.v1.SpawnStatus, Connect-JSON), stripped of
// the SPAWN_STATUS_ prefix for readability (e.g. "ACTIVE", "SUSPENDED").
export type SpawnStatus =
  | "UNSPECIFIED"
  | "STARTING"
  | "ACTIVE"
  | "SUSPENDING"
  | "SUSPENDED"
  | "UNREACHABLE"
  | "ERROR"
  | "DELETED"
  | "RESUMING";

export interface CreateSpawnOpts {
  appId: string;
  model?: string;
  name?: string;
  /** profileId attaches a customization profile at spawn-create (sp-tq0t.8). */
  profileId?: string;
}

export interface ForkOpts {
  name?: string;
  targetNodeId?: string;
  targetClass?: string;
}

/**
 * DriverCtx carries everything a driver call needs for one test: the per-worker identity, the
 * run namespacer, the apiDriver (used by every driver as the cross-check oracle), and — for
 * webDriver only — the Playwright page.
 */
export interface DriverCtx {
  identity: Identity;
  ns: (base: string) => string;
  api: ApiDriver;
  page?: unknown; // playwright Page; typed `unknown` here to avoid a hard @playwright/test dep in non-web drivers
}

export interface SpawnDriver {
  name: "web" | "cli";
  createSpawn(ctx: DriverCtx, opts: CreateSpawnOpts): Promise<SpawnId>;
  rename(ctx: DriverCtx, id: SpawnId, name: string): Promise<void>;
  setModel(ctx: DriverCtx, id: SpawnId, model: string): Promise<void>;
  suspend(ctx: DriverCtx, id: SpawnId): Promise<void>;
  resume(ctx: DriverCtx, id: SpawnId): Promise<void>;
  fork(ctx: DriverCtx, id: SpawnId, opts: ForkOpts): Promise<SpawnId>;
  stop(ctx: DriverCtx, id: SpawnId): Promise<void>;
  delete(ctx: DriverCtx, id: SpawnId): Promise<void>;
  /** waitActive polls/asserts until the spawn is rendered/reported ACTIVE (surface-appropriate). */
  waitActive(ctx: DriverCtx, id: SpawnId): Promise<void>;
  /** list returns the spawns visible on this surface for the current identity. */
  list(ctx: DriverCtx): Promise<{ spawnId: SpawnId; status: SpawnStatus; name: string }[]>;
}
