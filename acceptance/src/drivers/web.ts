/**
 * webDriver: Playwright page objects against the React SPA. Asserts RENDERED DOM state — that is
 * the primary check a UI acceptance suite exists to verify (design §Assertion strategy); the
 * apiDriver is a complementary cross-check, never the sole oracle.
 *
 * Selectors are anchored on existing data-testids (web/src/shell/Sidebar.tsx,
 * web/src/views/market/{Browse,Detail}.tsx, web/src/views/model/SetModelModal.tsx,
 * web/src/views/fork/ForkSpawnModal.tsx). Two are flagged below as the known soft spots not yet
 * exercised against a live target — grounded per the design's Phase 0/Phase 1 TDD seam (Phase 1's
 * first live run is expected to confirm or adjust them).
 */

import type { Page } from "@playwright/test";
import type { CreateSpawnOpts, DriverCtx, ForkOpts, SpawnDriver, SpawnId, SpawnStatus } from "./types";

function page(ctx: DriverCtx): Page {
  return ctx.page as Page;
}

/** parseSpawnIdFromUrl extracts the :id from a "/spawn/<id>" pathname (App.tsx's post-op nav target). */
function parseSpawnIdFromUrl(url: string): SpawnId {
  const m = /\/spawn\/([^/?#]+)/.exec(new URL(url).pathname);
  if (!m) throw new Error(`webDriver: expected the app to navigate to /spawn/<id>, got ${url}`);
  return decodeURIComponent(m[1]);
}

async function openKebabMenu(p: Page, id: SpawnId): Promise<void> {
  await p.getByTestId(`spawn-kebab-${id}`).click();
}

export class WebDriver implements SpawnDriver {
  readonly name = "web" as const;

  async createSpawn(ctx: DriverCtx, opts: CreateSpawnOpts): Promise<SpawnId> {
    const p = page(ctx);
    // The Detail view's "Spawn" button has no client-settable name field (product gap shared with
    // cliDriver's create — see api.ts's createSpawn for the one surface that CAN set it directly).
    // opts.name is intentionally unused here; call driver.rename() after creation if a scenario
    // needs a namespaced display name.
    await p.goto(`/templates/${encodeURIComponent(opts.appId)}`);
    await p.getByTestId("spawn-btn").click();
    await p.waitForURL(/\/spawn\//);
    return parseSpawnIdFromUrl(p.url());
  }

  async rename(ctx: DriverCtx, id: SpawnId, name: string): Promise<void> {
    const p = page(ctx);
    await p.getByTestId(`spawn-name-${id}`).dblclick();
    const input = p.getByTestId(`spawn-name-input-${id}`);
    await input.fill(name);
    await input.press("Enter");
  }

  async setModel(ctx: DriverCtx, id: SpawnId, model: string): Promise<void> {
    const p = page(ctx);
    await openKebabMenu(p, id);
    await p.getByTestId(`spawn-setmodel-${id}`).click();
    await p.locator("#model-input").fill(model);
    await p.getByTestId("set-model-submit").click();
  }

  private async lifecycleAction(ctx: DriverCtx, id: SpawnId, kind: "suspend" | "resume"): Promise<void> {
    const p = page(ctx);
    await openKebabMenu(p, id);
    await p.getByTestId(`spawn-${kind}-${id}`).click();
  }

  async suspend(ctx: DriverCtx, id: SpawnId): Promise<void> {
    await this.lifecycleAction(ctx, id, "suspend");
  }

  async resume(ctx: DriverCtx, id: SpawnId): Promise<void> {
    await this.lifecycleAction(ctx, id, "resume");
  }

  async fork(ctx: DriverCtx, id: SpawnId, opts: ForkOpts): Promise<SpawnId> {
    const p = page(ctx);
    await openKebabMenu(p, id);
    await p.getByTestId(`spawn-fork-${id}`).click();

    // ForkSpawnModal opens in a "loading" phase, then "selecting" — both render ForkNameInput once
    // ready. useForkSpawn pre-selects the current node as the default target, so "Continue" is
    // already enabled without picking a target explicitly — used below unless opts requests one.
    const nameInput = p.getByTestId("fork-name-input");
    await nameInput.waitFor({ state: "visible" });
    if (opts.name) await nameInput.fill(opts.name);

    // SOFT SPOT (not yet exercised live): a specific node/class pick clicks a per-target row;
    // "Continue" (no data-testid — matched by role/name) accepts the pre-selected default.
    if (opts.targetNodeId) {
      await p.getByTestId(`fork-target-${opts.targetNodeId}`).click();
    } else if (opts.targetClass) {
      await p.getByTestId(`fork-target-${opts.targetClass}`).click();
    } else {
      await p.getByRole("button", { name: "Continue" }).click();
    }

    await p.getByTestId("fork-confirm").click();
    await p.getByTestId("fork-done").waitFor({ state: "visible", timeout: 60_000 });
    await p.getByTestId("fork-open").click();
    await p.waitForURL(/\/spawn\//);
    return parseSpawnIdFromUrl(p.url());
  }

  async stop(ctx: DriverCtx, id: SpawnId): Promise<void> {
    const p = page(ctx);
    await openKebabMenu(p, id);
    await p.getByTestId(`spawn-stop-${id}`).click();
    await p.getByTestId(`spawn-stop-confirm-${id}`).click();
  }

  async delete(_ctx: DriverCtx, _id: SpawnId): Promise<void> {
    // DISCOVERED PRODUCT GAP (beyond the design's CLI-only parity note): the SpawnRow kebab menu
    // (web/src/shell/Sidebar.tsx) offers Rename/Suspend-Resume/MoveTo/Fork/SetModel/Stop but no
    // Delete — DeleteSpawn has no web UI affordance at all. Fail, don't skip, per this project's
    // "visible red over silent pass" convention (mirrors cliDriver's stubs).
    throw new Error("webDriver: no UI affordance to delete a spawn (product gap, see sp-tq0t)");
  }

  async waitActive(ctx: DriverCtx, id: SpawnId, opts: { timeoutMs?: number } = {}): Promise<void> {
    const p = page(ctx);
    // The active dot renders with data-status="active" (web/src/shell/Sidebar.tsx); wait for both
    // its presence and that specific status, since the row exists (as "starting") well before it.
    await p.waitForFunction(
      (spawnId) => {
        const el = document.querySelector(`[data-testid="spawn-dot-${spawnId}"]`);
        return el?.getAttribute("data-status") === "active";
      },
      id,
      { timeout: opts.timeoutMs ?? 60_000 },
    );
  }

  async list(ctx: DriverCtx): Promise<{ spawnId: SpawnId; status: SpawnStatus; name: string }[]> {
    const p = page(ctx);
    const rows = p.locator('[data-testid^="spawn-row-"]');
    const count = await rows.count();
    const out: { spawnId: SpawnId; status: SpawnStatus; name: string }[] = [];
    for (let i = 0; i < count; i++) {
      const row = rows.nth(i);
      const testid = await row.getAttribute("data-testid");
      const spawnId = testid?.replace(/^spawn-row-/, "") ?? "";
      const dataStatus = await p.getByTestId(`spawn-dot-${spawnId}`).getAttribute("data-status");
      const name = (await p.getByTestId(`spawn-name-${spawnId}`).textContent()) ?? "";
      out.push({ spawnId, status: (dataStatus?.toUpperCase() ?? "UNSPECIFIED") as SpawnStatus, name: name.trim() });
    }
    return out;
  }
}
