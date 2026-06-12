/**
 * Auth e2e: login via A1's fake GitHub provider + cross-origin /refresh cold-reload [AM2].
 *
 * HOST-GATED: set E2E_AUTH=1 to enable. Tests skip cleanly when the env var is absent.
 * Run with:
 *   E2E_AUTH=1 npx playwright test --config=playwright.auth.config.ts
 *
 * Infrastructure (started by global-setup-auth.ts):
 *   - AS (authsvc) at 127.0.0.1:8090 with AS_DEV=1 + AS_FAKE_GITHUB=1
 *   - CP + node on 8080 (same as the standard e2e suite)
 *   - Vite dev server on 5174 with VITE_AUTH_ENABLED=1 (proxies /oauth,/refresh to AS)
 *
 * Key invariant under test: on cold reload, bootstrap() reads the refresh_token_hash from
 * localStorage (persisted by setToken), calls /refresh with a valid PoP, and restores the
 * session WITHOUT requiring the user to log in again. Without the localStorage persistence
 * fix, bootstrap() would send a zero-hash PoP that the AS rejects (pop-bad-sig) → login wall.
 */

import { test, expect } from "@playwright/test";

const E2E_AUTH = !!process.env.E2E_AUTH;

test.describe("auth: login + /refresh cold-reload [AM2]", () => {
  test.describe.configure({ mode: "serial" });

  /**
   * Full OAuth login via fake GitHub provider: SPA shows login wall → user clicks
   * "Sign in with GitHub" → AS → fake GitHub (immediate redirect) → AS callback →
   * SPA parses access_token + refresh_token_hash, stores hash in localStorage, authed.
   */
  test("login via fake GitHub provider — SPA reaches templates page", async ({ page }) => {
    test.skip(!E2E_AUTH, "requires E2E_AUTH=1 (see playwright.auth.config.ts)");

    // Auth is enabled (VITE_AUTH_ENABLED=1): SPA shows loading → login wall.
    await page.goto("/");
    await expect(page.getByTestId("sign-in-btn")).toBeVisible({ timeout: 10_000 });

    // Click sign-in: SPA navigates to /oauth/authorize (proxied to AS) → AS redirects to
    // fake GitHub → fake GitHub immediately redirects to localhost:5174/oauth/callback →
    // AS sets refresh cookie (for localhost:5174 via proxy) and redirects to /callback?access_token=...
    // → SPA parses, stores refresh_token_hash in localStorage, navigates to /templates.
    await page.click('[data-testid="sign-in-btn"]');
    await expect(page).toHaveURL(/\/templates$/, { timeout: 30_000 });
    await expect(page.getByTestId("templates")).toBeVisible({ timeout: 10_000 });
  });

  /**
   * Cold-reload silent /refresh [AM2]: after login, a page reload must silently restore
   * the session via /refresh (using the persisted refresh_token_hash + PoP) without
   * showing the login wall. This directly validates the localStorage persistence fix:
   * without it, bootstrap() sends a zero-hash PoP → AS rejects → login wall appears.
   */
  test("cold reload restores session via /refresh without login wall [AM2]", async ({ page }) => {
    test.skip(!E2E_AUTH, "requires E2E_AUTH=1 (see playwright.auth.config.ts)");

    // Re-establish authed state (serial tests share browser context but reload clears memory).
    await page.goto("/");
    const loginBtn = page.getByTestId("sign-in-btn");
    if (await loginBtn.isVisible({ timeout: 3_000 }).catch(() => false)) {
      await loginBtn.click();
      await expect(page).toHaveURL(/\/templates$/, { timeout: 30_000 });
    }
    await expect(page.getByTestId("templates")).toBeVisible({ timeout: 10_000 });

    // Capture the /refresh call that bootstrap() issues on the next cold load.
    const refreshPromise = page.waitForRequest(/\/refresh/, { timeout: 20_000 });

    // Cold reload: clears in-memory session + zustand state; HttpOnly refresh cookie and
    // localStorage refresh_token_hash persist across the reload.
    await page.reload();

    // bootstrap() must have called /refresh.
    const refreshReq = await refreshPromise;
    expect(refreshReq.method()).toBe("POST");

    // Session is restored: templates page visible, login wall absent.
    await expect(page.getByTestId("templates")).toBeVisible({ timeout: 20_000 });
    await expect(loginBtn).not.toBeVisible({ timeout: 5_000 });
  });
});
