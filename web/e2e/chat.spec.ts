import { test, expect } from "@playwright/test";

test("chat echoes through the real browser", async ({ page }) => {
  await page.goto("/");

  // App spawns on mount + runs the ACP handshake; status flips to "ready".
  // Generous timeout absorbs the container cold start.
  await expect(page.locator(".status")).toHaveText("ready", { timeout: 40_000 });

  const token = "ping-" + Math.random().toString(36).slice(2, 8);
  await page.locator(".input textarea").fill("say " + token);
  await page.locator(".input button").click();

  // user echo bubble + the stub's "ECHO: <prompt>" agent bubble.
  await expect(page.locator(".bubble.user")).toContainText(token);
  await expect(page.locator(".bubble.agent")).toContainText("ECHO: say " + token, { timeout: 30_000 });
});
