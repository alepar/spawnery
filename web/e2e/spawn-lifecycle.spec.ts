import { test, expect, type Page } from "@playwright/test";
import { clearSpawns } from "./helpers";

test.beforeEach(async ({ request }) => { await clearSpawns(request); });

async function gotoApp(page: Page) {
  await page.goto("/");
  await expect(page.getByTestId("marketplace")).toBeVisible({ timeout: 20_000 });
}

// Spawn the seeded Secret App from the Marketplace WITHOUT reloading the page (preserves the
// client-side transcript buffer across instances). Call gotoApp first.
async function spawnFromMarket(page: Page) {
  await page.getByTestId("nav-market").click();
  const card = page.getByTestId("app-card-spawnery/secret-app");
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.click();
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });
  await page.getByTestId("spawn-btn").click();
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 40_000 });
}

// the spawn-row whose name span has EXACTLY `name` (avoids the "secret-app" ⊂ "secret-app 2" trap).
function rowByName(page: Page, name: string) {
  return page.locator('[data-testid^="spawn-row-"]').filter({ has: page.getByText(name, { exact: true }) }).first();
}

test("two instances of the same app coexist with distinct names + active dots", async ({ page }) => {
  await gotoApp(page);
  await spawnFromMarket(page);
  await spawnFromMarket(page);
  await expect(rowByName(page, "Secret App 2")).toBeVisible({ timeout: 20_000 });
  await expect(page.locator('[data-testid^="spawn-row-"]')).toHaveCount(2);
  await expect.poll(
    async () => page.locator('[data-testid^="spawn-dot-"][data-status="active"]').count(),
    { timeout: 20_000 },
  ).toBe(2);
});

test("rename a spawn from the sidebar", async ({ page }) => {
  await gotoApp(page);
  await spawnFromMarket(page);
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
  await spawnFromMarket(page);
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
  await spawnFromMarket(page);
  await expect(page.locator('[data-testid^="spawn-row-"]')).toHaveCount(1);
  const r = rowByName(page, "Secret App");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-stop-"]').click();          // arm confirm
  await r.locator('[data-testid^="spawn-stop-confirm-"]').click();  // confirm
  await expect(page.locator('[data-testid^="spawn-row-"]')).toHaveCount(0, { timeout: 20_000 });
});

test("switching between two spawns restores each transcript", async ({ page }) => {
  await gotoApp(page);
  // Create both instances first (spawnApp clears the item buffer; we must switch via selectSpawn to
  // save/restore transcripts — selectSpawn uses the functional updater that persists the buffer).
  await spawnFromMarket(page); // instance 1
  await spawnFromMarket(page); // instance 2 now active; instance 1 buffer = []

  // Explicitly switch to instance 1 via the sidebar row (selectSpawn saves instance 2's empty buf,
  // reconnects instance 1's ws, starts fresh transcript for instance 1).
  await rowByName(page, "Secret App").locator('[data-testid^="spawn-select-"]').click();
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 30_000 });

  await page.getByTestId("prompt-input").fill("say one");
  await page.getByTestId("prompt-send").click();
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });

  // Switch to instance 2 via row click — selectSpawn saves instance 1's transcript to its buffer.
  await rowByName(page, "Secret App 2").locator('[data-testid^="spawn-select-"]').click();
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 30_000 });
  await expect(page.locator('[data-role="agent"]')).toHaveCount(0);
  await page.getByTestId("prompt-input").fill("say two");
  await page.getByTestId("prompt-send").click();
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say two", { timeout: 30_000 });

  // Switch back to instance 1 — selectSpawn restores the saved transcript from the client buffer.
  await rowByName(page, "Secret App").locator('[data-testid^="spawn-select-"]').click();
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 20_000 });
  await expect(page.locator('[data-role="user"]')).toContainText("one");
});
