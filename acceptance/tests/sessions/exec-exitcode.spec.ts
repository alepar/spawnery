/**
 * cli-only: `spawnctl exec` exit-code propagation + stdout capture. No @agent involved (a plain
 * shell command in the spawn's container is enough), so no CostLedger, no retries:0 — just the
 * @mutating guardrail, since it still needs a live spawn.
 *
 * Precondition: ACC_TEST_APP_ID — a guaranteed-present spawn source. This scenario runs only
 * shell commands and deliberately does not claim the live-inference @agent capability.
 */

import { test, expect } from "../../src/harness/test";
import { waitActiveApi } from "./support";

test(
  "exec propagates exit code + captures stdout · cli",
  { tag: "@mutating" },
  async ({ api, cli, ctx, ns }) => {
    const appId = process.env.ACC_TEST_APP_ID;
    if (!appId) throw new Error("ACC_TEST_APP_ID is required for the non-LLM exec scenario");
    const id = await api.createSpawn({ appId, name: ns("exec") });
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
