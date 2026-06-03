import { test, expect, type Page } from "@playwright/test";
import { clearSpawns } from "./helpers";

test.beforeEach(async ({ request }) => { await clearSpawns(request); });

// Spawn the seeded "Secret App" from the Marketplace; it lands in the sidebar Spawns list and the
// chat opens. The stub agent echoes "ECHO: <prompt>".
async function spawnSecretApp(page: Page) {
  await page.goto("/");
  const card = page.getByTestId("app-card-spawnery/secret-app");
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.click();
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });
  await page.getByTestId("spawn-btn").click();
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 40_000 });
}

test("chat echoes through the real browser", async ({ page }) => {
  await spawnSecretApp(page);
  const token = "ping-" + Math.random().toString(36).slice(2, 8);
  await page.getByTestId("prompt-input").fill("say " + token);
  await page.getByTestId("prompt-send").click();
  await expect(page.locator('[data-role="user"]')).toContainText(token);
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say " + token, { timeout: 30_000 });
});

test("settings theme toggle flips dark mode without dropping the session", async ({ page }) => {
  await spawnSecretApp(page);
  const html = page.locator("html");
  const wasDark = await html.evaluate((el) => el.classList.contains("dark"));
  await page.getByTestId("nav-settings").click();
  await page.getByTestId("theme-toggle").click();
  await expect.poll(() => html.evaluate((el) => el.classList.contains("dark"))).toBe(!wasDark);
  // return to the spawn by clicking its row (exact display name "Secret App").
  await page.locator('[data-testid^="spawn-row-"]').filter({ has: page.getByText("Secret App", { exact: true }) }).first()
    .locator('[data-testid^="spawn-select-"]').click();
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 20_000 });
});
