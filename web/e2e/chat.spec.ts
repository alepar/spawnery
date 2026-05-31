import { test, expect } from "@playwright/test";

test("chat echoes through the real browser", async ({ page }) => {
  await page.goto("/");

  // App spawns on mount + runs the ACP handshake; status flips to "ready".
  // Generous timeout absorbs the container cold start.
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 40_000 });

  const token = "ping-" + Math.random().toString(36).slice(2, 8);
  await page.getByTestId("prompt-input").fill("say " + token);
  await page.getByTestId("prompt-send").click();

  // user echo bubble + the stub's "ECHO: <prompt>" agent bubble.
  await expect(page.locator('[data-role="user"]')).toContainText(token);
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say " + token, { timeout: 30_000 });
});

test("sidebar switches views and theme toggle flips dark mode without dropping the session", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 40_000 });

  const html = page.locator("html");
  const wasDark = await html.evaluate((el) => el.classList.contains("dark"));

  await page.getByTestId("nav-settings").click();
  await page.getByTestId("theme-toggle").click();
  await expect.poll(() => html.evaluate((el) => el.classList.contains("dark"))).toBe(!wasDark);

  // returning to chat shows the still-live session (status remains "ready").
  await page.getByTestId("nav-chat").click();
  await expect(page.getByTestId("status")).toHaveText("ready");
});
