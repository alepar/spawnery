import { test, expect, type Page } from "@playwright/test";
import { clearSpawns } from "./helpers";

test.beforeEach(async ({ request }) => { await clearSpawns(request); });

async function gotoApp(page: Page) {
  await page.goto("/");
  await expect(page).toHaveURL(/\/templates$/);
  await expect(page.getByTestId("templates")).toBeVisible({ timeout: 20_000 });
}

// Spawn the seeded Secret App from the Templates view WITHOUT reloading the page (preserves the
// client-side transcript buffer across instances). Call gotoApp first. After spawning, the shell is
// on the new spawn's chat (/spawn/<id>).
async function spawnFromTemplates(page: Page) {
  await page.getByTestId("nav-templates").click();
  await expect(page).toHaveURL(/\/templates$/);
  const card = page.getByTestId("app-card-spawnery/secret-app");
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.click();
  await expect(page).toHaveURL(/\/templates\/spawnery%2Fsecret-app$/);
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });
  await page.getByTestId("spawn-btn").click();
  await expect(page).toHaveURL(/\/spawn\/[^/]+$/);
  await expect(page.getByRole("banner").getByTestId("status")).toContainText("connected", { timeout: 40_000 });
}

// the spawn-row whose name span has EXACTLY `name` (avoids the "secret-app" ⊂ "secret-app 2" trap).
function rowByName(page: Page, name: string) {
  return page.locator('[data-testid^="spawn-row-"]').filter({ has: page.getByText(name, { exact: true }) }).first();
}

test("two instances of the same app coexist with distinct names + active dots", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page);
  await spawnFromTemplates(page);
  await expect(rowByName(page, "Secret App 2")).toBeVisible({ timeout: 20_000 });
  await expect(page.locator('[data-testid^="spawn-row-"]')).toHaveCount(2);
  await expect.poll(
    async () => page.locator('[data-testid^="spawn-dot-"][data-status="active"]').count(),
    { timeout: 20_000 },
  ).toBe(2);
});

test("rename a spawn from the sidebar", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page);
  const r = rowByName(page, "Secret App");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-rename-"]').click();
  // After clicking rename the "Secret App" span is replaced by an input, so the rowByName filter no
  // longer matches — use a page-level locator for the input directly.
  const input = page.locator('[data-testid^="spawn-name-input-"]');
  await input.fill("My Secret");
  await input.press("Enter");
  await expect(rowByName(page, "My Secret")).toBeVisible({ timeout: 10_000 });
});

test("suspend then resume a spawn", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page);
  const r = rowByName(page, "Secret App");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-suspend-"]').click();
  await expect.poll(
    async () => r.locator('[data-testid^="spawn-dot-"]').getAttribute("data-status"),
    { timeout: 20_000 },
  ).toBe("suspended");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-resume-"]').click();
  await expect.poll(
    async () => r.locator('[data-testid^="spawn-dot-"]').getAttribute("data-status"),
    { timeout: 30_000 },
  ).toBe("active");
});

test("stop removes the spawn from the list", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page);
  await expect(page.locator('[data-testid^="spawn-row-"]')).toHaveCount(1);
  const r = rowByName(page, "Secret App");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-stop-"]').click();          // arm confirm
  await r.locator('[data-testid^="spawn-stop-confirm-"]').click();  // confirm
  await expect(page.locator('[data-testid^="spawn-row-"]')).toHaveCount(0, { timeout: 20_000 });
});

test("switching between two spawns restores each transcript", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page); // instance 1 active
  await page.getByTestId("prompt-input").fill("say one");
  await page.getByTestId("prompt-input").press("Enter");
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });

  await spawnFromTemplates(page); // instance 2 active (no reload) — spawnApp must save instance 1's buffer
  await expect(page.locator('[data-role="agent"]')).toHaveCount(0);
  await page.getByTestId("prompt-input").fill("say two");
  await page.getByTestId("prompt-input").press("Enter");
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say two", { timeout: 30_000 });

  // switch back to instance 1 → its prior transcript is restored from the client buffer.
  await rowByName(page, "Secret App").locator('[data-testid^="spawn-select-"]').click();
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 20_000 });
  await expect(page.locator('[data-role="user"]')).toContainText("one");
});

test("conversation history survives a browser reload (node replay)", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page);
  await page.getByTestId("prompt-input").fill("say one");
  await page.getByTestId("prompt-input").press("Enter");
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });

  // The shell is on the spawn's chat after spawning; capture its deep-link URL.
  await expect(page).toHaveURL(/\/spawn\/[^/]+$/);
  const spawnUrl = page.url();

  // Reload wipes the client-side transcript buffer; the URL is the source of truth, so the same
  // /spawn/<id> view must re-bind on refresh and the node replays the transcript — no manual reopen.
  await page.reload();
  await expect(page).toHaveURL(spawnUrl);

  // On reconnect the pump replays its frame log from cursor 0 (a reset frame then the logged
  // frames) -> the prior transcript is restored.
  await expect(page.getByRole("banner").getByTestId("status")).toContainText("connected", { timeout: 40_000 });
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });
  await expect(page.locator('[data-role="user"]')).toContainText("one");
});

// Deep-link stability: a /spawn/<id> URL must cold-load straight onto that spawn's chat view (the
// right pane re-binds from the URL alone), proving refresh/bookmark/deep-link works.
test("deep-linking a /spawn/<id> URL cold-loads onto that spawn", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page);

  // Capture the spawn's URL + title, then navigate there fresh (a cold page.goto, not a reload).
  await expect(page).toHaveURL(/\/spawn\/[^/]+$/);
  const spawnUrl = page.url();
  await expect(page).toHaveTitle("Spawnery — Secret App");

  await page.goto(spawnUrl);
  await expect(page).toHaveURL(spawnUrl);
  // The right pane re-binds from the URL: status reconnects and the chat input renders.
  await expect(page.getByRole("banner").getByTestId("status")).toContainText("connected", { timeout: 40_000 });
  await expect(page.getByTestId("prompt-input")).toBeVisible();
  // Title resolves back to the spawn's name once the spawns poll lands.
  await expect(page).toHaveTitle("Spawnery — Secret App");
});

// Browser Back/Forward across the Templates -> spawn boundary.
test("browser back/forward moves between Templates and a spawn", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page); // now on /spawn/<id>
  await expect(page).toHaveURL(/\/spawn\/[^/]+$/);
  const spawnUrl = page.url();

  // Back: the spawn flow pushed templates -> app-detail -> spawn, so one goBack lands on the app
  // Detail (the page right before the spawn was created).
  await page.goBack();
  await expect(page).toHaveURL(/\/templates\/spawnery%2Fsecret-app$/);
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });

  // Forward: advance back onto the spawn's chat, which re-binds and reconnects.
  await page.goForward();
  await expect(page).toHaveURL(spawnUrl);
  await expect(page.getByRole("banner").getByTestId("status")).toContainText("connected", { timeout: 40_000 });
});

test("suspending a non-active spawn clears its cached transcript", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page); // instance 1 active
  await page.getByTestId("prompt-input").fill("say one");
  await page.getByTestId("prompt-input").press("Enter");
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });

  await spawnFromTemplates(page); // instance 2 active (no reload) — instance 1 is now non-active, buffer saved
  await expect(page.locator('[data-role="agent"]')).toHaveCount(0);

  // Suspend instance 1 (NOT the active spawn) from its sidebar kebab.
  const r1 = rowByName(page, "Secret App");
  await r1.locator('[data-testid^="spawn-kebab-"]').click();
  await r1.locator('[data-testid^="spawn-suspend-"]').click();
  await expect.poll(
    async () => r1.locator('[data-testid^="spawn-dot-"]').getAttribute("data-status"),
    { timeout: 20_000 },
  ).toBe("suspended");

  // Re-select the suspended instance 1: its cached transcript was cleared on suspend, and a suspended
  // spawn does not reconnect, so the chat shows nothing.
  await r1.locator('[data-testid^="spawn-select-"]').click();
  await expect(page.locator('[data-role="agent"]')).toHaveCount(0, { timeout: 20_000 });
  await expect(page.locator('[data-role="user"]')).toHaveCount(0, { timeout: 20_000 });
});

test("suspending the active spawn clears its on-screen transcript", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page);
  await page.getByTestId("prompt-input").fill("say one");
  await page.getByTestId("prompt-input").press("Enter");
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });

  // Suspend the currently-active spawn from the sidebar kebab.
  const r = rowByName(page, "Secret App");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-suspend-"]').click();

  // A resumed spawn starts fresh, so the stale transcript is wiped immediately.
  await expect(page.locator('[data-role="agent"]')).toHaveCount(0, { timeout: 20_000 });
  await expect(page.locator('[data-role="user"]')).toHaveCount(0, { timeout: 20_000 });
});

// Enforces our non-standard ACP model: ONE long-lived agent process serves MANY client sessions, so
// reconnecting (reload + reopen) re-sends `initialize` + `session/new` to an already-initialized agent.
// The agent must tolerate the repeated handshake AND still serve a fresh turn. Verified against goose
// (the handshake re-answers cleanly); this guards the property for the stub + relay and for any future
// agent added to the e2e lane. Making it spec-compliant (node owns one session) is sp-r7t.
test("a reconnected spawn still serves new prompts (repeated initialize tolerated)", async ({ page }) => {
  await gotoApp(page);
  await spawnFromTemplates(page);
  await page.getByTestId("prompt-input").fill("say one");
  await page.getByTestId("prompt-input").press("Enter");
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });

  // Reload on the spawn's URL -> fresh ACP handshake against the same still-running agent. The URL is
  // authoritative, so the spawn re-binds from it (no manual reopen). Reaching "connected" proves the
  // repeated `initialize`/`session/new` did NOT error.
  const spawnUrl = page.url();
  await expect(page).toHaveURL(/\/spawn\/[^/]+$/);
  await page.reload();
  await expect(page).toHaveURL(spawnUrl);
  await expect(page.getByRole("banner").getByTestId("status")).toContainText("connected", { timeout: 40_000 });

  // A NEW turn on the reconnected session must work end-to-end. Scope to the new bubble: by now the
  // replayed "ECHO: say one" is also on screen, so a bare [data-role="agent"] match is non-unique.
  await page.getByTestId("prompt-input").fill("say two");
  await page.getByTestId("prompt-input").press("Enter");
  await expect(
    page.locator('[data-role="agent"]').filter({ hasText: "ECHO: say two" }),
  ).toBeVisible({ timeout: 30_000 });
});
