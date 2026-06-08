import { test, expect } from "@playwright/test";
import { clearSpawns } from "./helpers";

test.beforeEach(async ({ request }) => { await clearSpawns(request); });

test("templates browse→detail→spawn flow", async ({ page }) => {
  await page.goto("/");

  // "/" normalizes to /templates (Browse is the default view; no auto-spawn). global-setup seeds the
  // reviewed catalog (spawnery/wiki etc.). The URL + title reflect the Templates section.
  await expect(page).toHaveURL(/\/templates$/);
  await expect(page).toHaveTitle("Spawnery — Templates");
  const wikiCard = page.getByTestId("app-card-spawnery/wiki");
  await expect(wikiCard).toBeVisible({ timeout: 20_000 });

  // Click the card → Detail view should render with a spawn button; the URL becomes /templates/<appId>.
  await wikiCard.click();
  await expect(page).toHaveURL(/\/templates\/spawnery%2Fwiki$/);
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });
  // Detail owns the title once the app fetch resolves: it becomes the human title (starts "Wiki…").
  await expect(page).toHaveTitle(/^Spawnery — Wiki/);

  // Spawn the app → the shell navigates to the spawn's chat (/spawn/<id>) and status flips to ready.
  await page.getByTestId("spawn-btn").click();
  await expect(page).toHaveURL(/\/spawn\/[^/]+$/);

  // Auto-wait: status must reach "connected" (new spawnlet session).
  await expect(page.getByRole("banner").getByTestId("status")).toContainText("connected", { timeout: 40_000 });

  // Confirm the chat input is available (user can immediately talk to the app).
  await expect(page.getByTestId("prompt-input")).toBeVisible();
});

test("templates browse tab shows app grid and detail back-button returns to browse", async ({ page }) => {
  await page.goto("/");

  // Browse is the default view now (no auto-spawn).
  await expect(page).toHaveURL(/\/templates$/);
  await expect(page.getByTestId("templates")).toBeVisible({ timeout: 20_000 });

  // The browse tab is active by default; verify the tab button exists.
  await expect(page.getByTestId("templates-tab-browse")).toBeVisible();

  // Wait for at least one card (prefix match).
  await expect(page.getByTestId("app-card-spawnery/wiki")).toBeVisible({ timeout: 20_000 });

  // Open detail → URL + title reflect the app.
  await page.getByTestId("app-card-spawnery/wiki").click();
  await expect(page).toHaveURL(/\/templates\/spawnery%2Fwiki$/);
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });

  // Go back to browse via the back button → URL + title return to Templates.
  await page.getByTestId("detail-back").click();
  await expect(page).toHaveURL(/\/templates$/);
  await expect(page).toHaveTitle("Spawnery — Templates");
  await expect(page.getByTestId("app-card-spawnery/wiki")).toBeVisible({ timeout: 10_000 });
});

// Browser Back/Forward must move between the URL-authoritative views and re-render each one.
test("browser back/forward moves between Browse and app Detail", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/\/templates$/);
  const wikiCard = page.getByTestId("app-card-spawnery/wiki");
  await expect(wikiCard).toBeVisible({ timeout: 20_000 });

  // Browse -> Detail (a history push).
  await wikiCard.click();
  await expect(page).toHaveURL(/\/templates\/spawnery%2Fwiki$/);
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });

  // Back -> Browse: URL + rendered grid return to the previous entry.
  await page.goBack();
  await expect(page).toHaveURL(/\/templates$/);
  await expect(page).toHaveTitle("Spawnery — Templates");
  await expect(wikiCard).toBeVisible({ timeout: 10_000 });

  // Forward -> Detail again: URL advances and the detail pane re-renders.
  await page.goForward();
  await expect(page).toHaveURL(/\/templates\/spawnery%2Fwiki$/);
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });
});
