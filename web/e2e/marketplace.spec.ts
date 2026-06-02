import { test, expect } from "@playwright/test";

test("marketplace browse→detail→spawn flow", async ({ page }) => {
  await page.goto("/");

  // Marketplace is the default view now (no auto-spawn). Browse is the default tab; global-setup
  // seeds the reviewed catalog (spawnery/wiki etc.). Wait for a seeded card to appear.
  const wikiCard = page.getByTestId("app-card-spawnery/wiki");
  await expect(wikiCard).toBeVisible({ timeout: 20_000 });

  // Click the card → Detail view should render with a spawn button.
  await wikiCard.click();
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });

  // Spawn the app → the shell navigates back to chat and the status flips to "ready".
  await page.getByTestId("spawn-btn").click();

  // Auto-wait: status must reach "ready" again (new spawnlet session).
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 40_000 });

  // Confirm the chat input is available (user can immediately talk to the app).
  await expect(page.getByTestId("prompt-input")).toBeVisible();
});

test("marketplace browse tab shows app grid and detail back-button returns to browse", async ({ page }) => {
  await page.goto("/");

  // Marketplace is the default view now (no auto-spawn).
  await expect(page.getByTestId("marketplace")).toBeVisible({ timeout: 20_000 });

  // The browse tab is active by default; verify the tab button exists.
  await expect(page.getByTestId("market-tab-browse")).toBeVisible();

  // Wait for at least one card (prefix match).
  await expect(page.getByTestId("app-card-spawnery/wiki")).toBeVisible({ timeout: 20_000 });

  // Open detail.
  await page.getByTestId("app-card-spawnery/wiki").click();
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });

  // Go back to browse via the back button.
  await page.getByTestId("detail-back").click();
  await expect(page.getByTestId("app-card-spawnery/wiki")).toBeVisible({ timeout: 10_000 });
});
