/**
 * @agent sessions: prompt → rendered transcript + multi-tab/reload persistence + exec
 * side-effect. Because the unary apiDriver cannot observe the bidi Session RPC / SPA WebSocket
 * (design §Assertion strategy), this asserts the RENDERED transcript structurally (never agent
 * prose) and the session history surviving a full page reload, plus the agent's real side effect
 * (a per-run-unique marker file) read back fresh via `spawnctl exec`.
 *
 * Preconditions: ACC_AGENT_APP_ID (a real coding-agent app on the target) and ACC_AGENT_MODEL (a
 * pinned, cheap model) — missing either FAILS this spec loudly (agentAppId/agentModel throw),
 * never skips. See acceptance/README.md.
 */

import type { Page } from "@playwright/test";
import { test, expect } from "../../src/harness/test";
import { SessionPage } from "../../src/drivers/session";
import { agentAppId, agentModel, newMarker, recordAgentCost, waitActiveApi } from "./support";

const MARKER_FILE = "acc-marker.txt";

test.describe("@agent sessions: prompt → rendered transcript + reload + exec side-effect", () => {
  // Agent turns never auto-retry — a real-LLM-agent failure masks regressions and multiplies
  // OpenRouter cost (design §NFRs); the suite's default retries:2 is for infra flake only.
  test.describe.configure({ retries: 0 });

  test(
    "prompt renders a structural transcript, survives reload, and its exec side-effect is fresh · web+cli",
    { tag: ["@agent", "@mutating"] },
    async ({ target, api, cli, ctx, page, ledger, ns }) => {
      const appId = agentAppId(target);
      const model = agentModel(target);
      const estimate = Number(process.env.ACC_AGENT_TOKEN_ESTIMATE ?? "2000");

      const marker = newMarker(target.runId!, "prompt-transcript");
      const id = await api.createSpawn({ appId, model, name: ns("sess") });
      await waitActiveApi(api, id);

      const sp = new SessionPage(ctx.page as Page);
      await sp.open(id);

      const prompt = `Create a file at /workspace/${MARKER_FILE} containing exactly: ${marker} — then reply done.`;
      await sp.sendPrompt(prompt);
      await sp.waitTurnSettled();

      // Structural web asserts — NEVER agent prose (design line 145).
      expect(await sp.userMessages().count()).toBeGreaterThan(0);
      expect((await sp.userMessages().last().textContent()) ?? "").toContain("Create a file at");
      expect(await sp.agentMessages().count()).toBeGreaterThan(0);

      // Reload persistence: session history is reconstructed server-side without the live stream
      // — the black-box proof it survives a full page reload / a fresh tab.
      await page.reload();
      await page.getByTestId("prompt-input").waitFor({ state: "visible" });
      expect(await sp.userMessages().count()).toBeGreaterThan(0);
      expect(await sp.agentMessages().count()).toBeGreaterThan(0);
      expect((await sp.userMessages().last().textContent()) ?? "").toContain("Create a file at");

      // cli side-effect: a FRESH per-run marker, so a stale file from a prior run can't pass
      // (design line 143).
      const r = await cli.exec(ctx, id, ["cat", `/workspace/${MARKER_FILE}`]);
      expect(r.code).toBe(0);
      expect(r.stdout).toContain(marker);

      await recordAgentCost(sp, ledger, estimate);

      // Complementary oracle cross-check (apiDriver, surface-agnostic).
      expect(await api.listSpawns()).toContainSpawn(id, { status: "ACTIVE" });
    },
  );
});
