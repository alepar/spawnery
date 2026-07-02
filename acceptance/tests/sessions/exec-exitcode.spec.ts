/**
 * cli-only: `spawnctl exec` exit-code propagation + stdout capture. No @agent involved (a plain
 * shell command in the spawn's container is enough), so no CostLedger, no retries:0 — just the
 * @mutating guardrail, since it still needs a live spawn.
 *
 * Precondition: ACC_AGENT_APP_ID — reuses the agent app as a guaranteed-present spawn source
 * (same precondition as the sibling @agent spec; documented in acceptance/README.md).
 */

import { test, expect } from "../../src/harness/test";
import { agentAppId, waitActiveApi } from "./support";

test(
  "exec propagates exit code + captures stdout · cli",
  { tag: "@mutating" },
  async ({ target, api, cli, ctx, ns }) => {
    const id = await api.createSpawn({ appId: agentAppId(target), name: ns("exec") });
    await waitActiveApi(api, id);

    const ok = await cli.exec(ctx, id, ["sh", "-c", "exit 0"]);
    expect(ok.code).toBe(0);

    const bad = await cli.exec(ctx, id, ["sh", "-c", "exit 3"]);
    expect(bad.code).toBe(3);

    const out = await cli.exec(ctx, id, ["sh", "-c", "printf hello"]);
    expect(out.code).toBe(0);
    expect(out.stdout).toContain("hello");
  },
);
