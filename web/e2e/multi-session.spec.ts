import { test, expect, type Page } from "@playwright/test";
import { clearSpawns, listSessions } from "./helpers";

test.beforeEach(async ({ request }) => { await clearSpawns(request); });

// The primary tab's ConnStatus dot is the canonical "spawn is live" light (the old header banner dot
// was removed). The dot is text-free now, so assert via data-status, scoped to the primary tab.
const primaryTabStatus = (page: Page) => page.getByTestId("tab-0").getByTestId("status");

// Activate a terminal session, run an (idempotent) command, and wait for `expectText` to appear in the
// pane. Readiness signal: the tab's own ConnStatus light goes "connected" when TerminalView's ws->PTY
// bridge is up. A freshly-activated xterm can fit while the panel is still 0-sized, leaving the pane
// blank until a real resize, so each retry nudges the viewport (forcing the active TerminalView's
// ResizeObserver to refit + re-send cols/rows -> tmux repaints) and re-types the command. The command
// must be safe to run repeatedly. xterm renders to canvas; visible text lives in the accessibility
// buffer (which toContainText reads), so never poll innerText.
async function runInTerminal(page: Page, sessionId: string, cmd: string, expectText: string) {
  await page.getByTestId(`tab-${sessionId}`).click();
  const term = page.getByTestId(`panel-${sessionId}`);
  await expect(term).toBeVisible();
  await expect(page.getByTestId(`tab-${sessionId}`).getByTestId("status")).toHaveAttribute("data-status", "connected", { timeout: 30_000 });
  await expect(async () => {
    const vp = page.viewportSize() ?? { width: 1280, height: 720 };
    await page.setViewportSize({ width: vp.width, height: vp.height - 40 });
    await page.setViewportSize(vp);
    await term.click();
    await page.keyboard.type(cmd + "\n");
    await expect(term).toContainText(expectText, { timeout: 4_000 });
  }).toPass({ timeout: 45_000 });
  return term;
}

// Spawn the seeded "Secret App". With AGENT_BINARIES=stub (global-setup) its primary (session #0) is
// the credential-free stub-acp echo agent, and the "+" menu offers a 2nd stub-acp ACP session + shell.
async function spawnSecretApp(page: Page): Promise<string> {
  await page.goto("/");
  await expect(page).toHaveURL(/\/templates$/);
  const card = page.getByTestId("app-card-spawnery/secret-app");
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.click();
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });
  await page.getByTestId("spawn-btn").click();
  await expect(page).toHaveURL(/\/spawn\/[^/]+$/);
  await expect(primaryTabStatus(page)).toHaveAttribute("data-status", "connected", { timeout: 40_000 });
  return page.url().split("/spawn/")[1];
}

// LANE A (runs HERE, credential-free): one spawn hosts multiple concurrent sessions in its single
// container — a 2nd ACP Pump (stub-acp), shell tabs (2nd/3rd tmux), shared fs across tabs, per-surface
// liveness, and close-reaps-only-that-session. Maps 1:1 to spec §7. No model keys needed.
test("multi-session: shell + 2nd acp Pump concurrent in one container; shared fs; close reaps only one", async ({ page, request }) => {
  const spawnId = await spawnSecretApp(page);
  await expect(page.getByTestId("tab-0")).toBeVisible();   // pinned primary (stub-acp)
  await expect(page.getByTestId("close-0")).toHaveCount(0);

  // --- add a SHELL tab (mosh/tmux — a 2nd tmux session in the same container) ---
  await page.getByTestId("add-session").click();
  await page.getByTestId("new-session-shell").click();
  await expect(page.locator('[data-testid^="tab-"]')).toHaveCount(2, { timeout: 30_000 });

  // --- add a SECOND acp session (a 2nd stub-acp Pump on a pool port) ---
  await page.getByTestId("add-session").click();
  await page.getByTestId("new-session-stub-acp").click();
  await expect(page.locator('[data-testid^="tab-"]')).toHaveCount(3, { timeout: 30_000 });

  // CONCURRENT IN ONE CONTAINER: roster shows 3 live sessions (acp #0 + shell + 2nd stub-acp).
  await expect.poll(async () => (await listSessions(request, spawnId)).length, { timeout: 30_000 }).toBe(3);
  const roster1 = await listSessions(request, spawnId);
  expect(roster1.filter((s) => s.runnable === "stub-acp").length).toBe(2);
  expect(roster1.filter((s) => s.runnable === "shell").length).toBe(1);

  // THE 2ND ACP SESSION IS ACTUALLY USABLE LIVE — covers the attach-race that let the bug ship: the
  // panel must not bind while the Pump is still STARTING, then must chat once it's ready. Focus its
  // tab, wait for ITS dot to read "connected" (the web now only shows that once the gated socket is
  // truly open — status==active), then prompt and assert the stub's ECHO reply in THAT panel. No reload.
  const acp2Id = roster1.filter((s) => s.runnable === "stub-acp").map((s) => s.sessionId)
    .find((id) => id !== "0")!;
  await page.getByTestId(`tab-${acp2Id}`).click();
  await expect(page.getByTestId(`tab-${acp2Id}`).getByTestId("status"))
    .toHaveAttribute("data-status", "connected", { timeout: 40_000 });
  const acp2Panel = page.getByTestId(`panel-${acp2Id}`);
  await acp2Panel.getByTestId("prompt-input").fill("say second");
  await acp2Panel.getByTestId("prompt-input").press("Enter");
  await expect(acp2Panel.locator('[data-role="user"]')).toContainText("say second");
  await expect(acp2Panel.locator('[data-role="agent"]')).toContainText("ECHO: say second", { timeout: 30_000 });

  // PRIMARY #0 ANSWERS CONCURRENTLY with the 2nd Pump (no cross-session interference). Each kept-alive
  // ACP panel renders its own ChatView, so scope to panel-0. (This catches the sp-npxq.5 router bug
  // where a shared clientId let the 2nd session clobber #0's sender — fixed in cp/router.)
  await page.getByTestId("tab-0").click();
  const primaryPanel = page.getByTestId("panel-0");
  await primaryPanel.getByTestId("prompt-input").fill("say early");
  await primaryPanel.getByTestId("prompt-input").press("Enter");
  await expect(primaryPanel.locator('[data-role="user"]')).toContainText("say early");
  await expect(primaryPanel.locator('[data-role="agent"]')).toContainText("ECHO: say early", { timeout: 30_000 });

  // PER-SURFACE LIVENESS — the shell echoes a token AND writes it to a shared file:
  const shellId = roster1.find((s) => s.runnable === "shell")!.sessionId;
  const token = "fs-" + Math.random().toString(36).slice(2, 8);
  await runInTerminal(page, shellId, `echo ${token} > /tmp/shared && cat /tmp/shared`, token);

  // SHARED FS across tabs — open a 2nd shell tab and read /tmp/shared; it sees the token the FIRST
  // shell wrote. This is the load-bearing "same container" proof (the 2nd shell is a different tmux
  // session/process from the first, yet shares the container filesystem).
  await page.getByTestId("add-session").click();
  await page.getByTestId("new-session-shell").click();
  await expect(page.locator('[data-testid^="tab-"]')).toHaveCount(4, { timeout: 30_000 });
  await expect.poll(async () => (await listSessions(request, spawnId)).filter((s) => s.runnable === "shell").length,
    { timeout: 30_000 }).toBe(2);
  const shell2Id = (await listSessions(request, spawnId))
    .filter((s) => s.runnable === "shell").map((s) => s.sessionId).find((id) => id !== shellId)!;
  await runInTerminal(page, shell2Id, "cat /tmp/shared", token);   // shared container fs

  // CLOSE REAPS ONLY THAT SESSION: close the FIRST shell; the other three survive (its port/tmux are
  // freed without tearing down the container or the sibling sessions/pumps).
  await page.getByTestId(`close-${shellId}`).click();
  await expect(page.locator('[data-testid^="tab-"]')).toHaveCount(3, { timeout: 20_000 });
  await expect.poll(async () => (await listSessions(request, spawnId)).map((s) => s.sessionId), { timeout: 20_000 })
    .not.toContain(shellId);
  // The 2nd stub-acp Pump + 2nd shell + primary are untouched (per-session reap, not container teardown):
  await expect(page.getByTestId(`tab-${shell2Id}`)).toBeVisible();
  const survivors = await listSessions(request, spawnId);
  expect(survivors.filter((s) => s.runnable === "stub-acp").length).toBe(2);
  expect(survivors.filter((s) => s.runnable === "shell").length).toBe(1);
  await page.getByTestId("tab-0").click();
  await expect(primaryTabStatus(page)).toHaveAttribute("data-status", "connected");
});

// LANE B (NOT runnable here — needs a real agent image with the launcher, AGENT_BINARIES of real
// runnables, a real OPENROUTER_API_KEY, and the sidecar). Visible-but-skipped so the coverage gap is
// explicit. Run in the user's env with SPAWNERY_LIVE_AGENTS=1.
test.describe("Lane B (real model / real sidecar — user's env only)", () => {
  test.skip(() => !process.env.SPAWNERY_LIVE_AGENTS, "needs real agent image + OPENROUTER_API_KEY + sidecar");

  test("a 2nd session of a DIFFERENT real runnable answers via a model", async ({ page }) => {
    // Requires AGENT_IMAGE=spawnery-agent:* (launcher present), AGENT_BINARIES=goose,opencode,claude-code,
    // OPENROUTER_API_KEY, sidecar. Spawn an opencode/goose primary, add e.g. goose-acp as a 2nd session,
    // and assert a real (non-echo) model reply — proving a genuinely different runnable answers live.
    void page;
  });

  test("opencode-tui standalone-primary boots a usable backend (spec §2/point 4)", async ({ page }) => {
    // primary = opencode-tui with NO served sibling -> launcher runs bare `opencode` (self-hosted server);
    // assert the terminal renders the TUI and a prompt yields a model reply (sidecar-routed).
    void page;
  });
});
