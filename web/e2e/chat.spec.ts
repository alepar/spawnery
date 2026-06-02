import { test, expect, type Page } from "@playwright/test";

// No auto-spawn anymore: the app opens on the Marketplace (default view). To get a live chat session
// we spawn a seeded app — "Secret App" (spawnery/secret-app), run by the stub agent which echoes
// "ECHO: <prompt>". The spawned app then lives under the sidebar "Spawns" section (nav-spawn).
async function spawnFromMarketplace(page: Page, cardId = "app-card-spawnery/secret-app") {
  await page.goto("/");
  const card = page.getByTestId(cardId);
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.click();
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });
  await page.getByTestId("spawn-btn").click();
  // ACP handshake; generous timeout absorbs the container cold start.
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 40_000 });
}

test("chat echoes through the real browser", async ({ page }) => {
  await spawnFromMarketplace(page);

  const token = "ping-" + Math.random().toString(36).slice(2, 8);
  await page.getByTestId("prompt-input").fill("say " + token);
  await page.getByTestId("prompt-send").click();

  // user echo bubble + the stub's "ECHO: <prompt>" agent bubble.
  await expect(page.locator('[data-role="user"]')).toContainText(token);
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say " + token, { timeout: 30_000 });
});

test("sidebar switches views and theme toggle flips dark mode without dropping the session", async ({ page }) => {
  await spawnFromMarketplace(page);

  const html = page.locator("html");
  const wasDark = await html.evaluate((el) => el.classList.contains("dark"));

  await page.getByTestId("nav-settings").click();
  await page.getByTestId("theme-toggle").click();
  await expect.poll(() => html.evaluate((el) => el.classList.contains("dark"))).toBe(!wasDark);

  // returning to the active spawn (the sidebar "Spawns" item) shows the still-live session.
  await page.getByTestId("nav-spawn").click();
  await expect(page.getByTestId("status")).toHaveText("ready");
});
