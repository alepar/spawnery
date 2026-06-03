# Suspend Clears a Spawn's Message History — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a spawn is suspended (on success), drop its message history from React state — the cached transcript buffer always, and the on-screen transcript too when the suspended spawn is the active one.

**Architecture:** A two-line change to the `onSuspend` handler in `web/src/App.tsx`. Message history lives in two places: `buffersRef` (a `Map<spawnId, Item[]>` caching transcripts for non-active spawns) and `items` (the visible transcript of the active spawn). On a successful suspend we `buffersRef.current.delete(id)` always, and `setItems([])` when the suspended spawn is active. Resumed spawns start fresh, so the retained transcript is stale.

**Tech Stack:** React 19 + TypeScript, Vite. Tested with Playwright (`web/e2e/`) — the suite is self-contained (Playwright starts Vite via `webServer` and the stub spawnlet via `globalSetup`).

---

## Why e2e (not a unit test)

`web/src/App.tsx` is integration glue (WebSocket sessions, ledger polling, ref-mirrored state) and has **no** unit tests by design. Every transcript-buffer behavior in this codebase is covered in `web/e2e/spawn-lifecycle.spec.ts` ("switching between two spawns restores each transcript", "conversation history survives a browser reload", "suspend then resume a spawn"). We follow that established pattern rather than introduce a new App-level RTL harness.

**Spec test case 3 (failed suspend leaves history intact)** is not e2e-tested: the self-contained stub gives no fault-injection seam to force `suspendSpawn` to reject. It is instead guaranteed structurally — both clearing statements live *inside* the `try`, *after* the `await suspendSpawn(id)` resolves, so a rejected call skips them entirely. This is verified by code inspection in Step 4, not a test.

## File Structure

- **Modify:** `web/src/App.tsx` — `onSuspend` handler (currently lines 178–184).
- **Test:** `web/e2e/spawn-lifecycle.spec.ts` — add two tests next to the existing "suspend then resume a spawn" test. Reuse the file's existing `gotoApp`, `spawnFromMarket`, and `rowByName` helpers (already defined at the top of the file — do not redefine them).

Both tests drive the same one-change fix: one exercises the buffer-delete line (non-active spawn), the other the `setItems([])` line (active spawn). They are written together, fail together, and pass together after the single edit — so this is one task, not two.

---

## Task 1: Suspend clears message history

**Files:**
- Modify: `web/src/App.tsx:178-184` (the `onSuspend` handler)
- Test: `web/e2e/spawn-lifecycle.spec.ts` (append two tests)

All commands run from the `web/` directory.

- [ ] **Step 1: Write the failing test for the non-active (cached buffer) case**

Append this test to `web/e2e/spawn-lifecycle.spec.ts` (after the existing "suspend then resume a spawn" test). It spawns two instances, suspends the *non-active* one, then re-selects it — a suspended spawn does not reconnect (no node replay), so the only thing that could show its old messages is the stale cached buffer.

```ts
test("suspending a non-active spawn clears its cached transcript", async ({ page }) => {
  await gotoApp(page);
  await spawnFromMarket(page); // instance 1 active
  await page.getByTestId("prompt-input").fill("say one");
  await page.getByTestId("prompt-send").click();
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });

  await spawnFromMarket(page); // instance 2 active (no reload) — instance 1 is now non-active, buffer saved
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
  await expect(page.locator('[data-role="user"]')).toHaveCount(0);
});
```

- [ ] **Step 2: Write the failing test for the active (on-screen transcript) case**

Append this test directly after the one from Step 1. It suspends the spawn that is currently selected/on-screen and asserts the visible transcript empties immediately.

```ts
test("suspending the active spawn clears its on-screen transcript", async ({ page }) => {
  await gotoApp(page);
  await spawnFromMarket(page);
  await page.getByTestId("prompt-input").fill("say one");
  await page.getByTestId("prompt-send").click();
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });

  // Suspend the currently-active spawn from the sidebar kebab.
  const r = rowByName(page, "Secret App");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-suspend-"]').click();

  // A resumed spawn starts fresh, so the stale transcript is wiped immediately.
  await expect(page.locator('[data-role="agent"]')).toHaveCount(0, { timeout: 20_000 });
  await expect(page.locator('[data-role="user"]')).toHaveCount(0);
});
```

- [ ] **Step 3: Run the two new tests to verify they fail**

Run:
```bash
npx playwright test spawn-lifecycle.spec.ts -g "clears its cached transcript|clears its on-screen transcript"
```
Expected: BOTH FAIL.
- "non-active … cached transcript": after re-selecting instance 1, `[data-role="agent"]` still contains "ECHO: say one" (buffer not cleared) → `toHaveCount(0)` fails.
- "active … on-screen transcript": after suspend, the agent/user bubbles are still on screen (`items` not cleared) → `toHaveCount(0)` fails.

- [ ] **Step 4: Apply the fix to `onSuspend`**

In `web/src/App.tsx`, replace the `onSuspend` handler (lines 178–184):

```js
  const onSuspend = async (id: string) => {
    try {
      await suspendSpawn(id);
      if (activeIdRef.current === id) { closeSession(); }
    } catch (e: any) { toast.error("Suspend failed: " + e.message); }
    refreshSpawns();
  };
```

with:

```js
  const onSuspend = async (id: string) => {
    try {
      await suspendSpawn(id);
      buffersRef.current.delete(id); // resumed spawns start fresh — drop the stale cached transcript
      if (activeIdRef.current === id) { closeSession(); setItems([]); }
    } catch (e: any) { toast.error("Suspend failed: " + e.message); }
    refreshSpawns();
  };
```

Both new statements sit inside the `try`, after the `await`, so a failed suspend clears nothing (spec test case 3, guaranteed structurally). `setItems` and `buffersRef` are already in scope (declared at the top of `App`).

- [ ] **Step 5: Run the two new tests to verify they pass**

Run:
```bash
npx playwright test spawn-lifecycle.spec.ts -g "clears its cached transcript|clears its on-screen transcript"
```
Expected: BOTH PASS.

- [ ] **Step 6: Run the full e2e lifecycle suite to check for regressions**

Run:
```bash
npm run test:e2e -- spawn-lifecycle.spec.ts
```
Expected: PASS (all tests, including the pre-existing "suspend then resume a spawn", "switching between two spawns restores each transcript", and "conversation history survives a browser reload"). The fix touches only the suspend path, so reload/replay and select-restore behavior are unaffected.

- [ ] **Step 7: Run the unit test suite to confirm nothing else broke**

Run:
```bash
npm test
```
Expected: PASS (no `App.tsx` unit tests exist; this confirms the edit didn't break typecheck-adjacent imports or other suites).

- [ ] **Step 8: Commit**

```bash
git add web/src/App.tsx web/e2e/spawn-lifecycle.spec.ts
git commit -m "fix(web): clear a spawn's message history on suspend

Resumed spawns start fresh, so drop the cached transcript buffer (and the
on-screen transcript when the suspended spawn is active) on a successful
suspend. Clearing happens inside the try after the await, so a failed
suspend leaves history intact.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:**
- "Clear cached buffer on suspend" → Task 1, Step 1 test + Step 4 (`buffersRef.current.delete(id)`). ✓
- "Clear on-screen transcript when active" → Task 1, Step 2 test + Step 4 (`setItems([])`). ✓
- "Clear only on success" → Step 4 placement inside `try` after `await`; rationale documented under "Why e2e". ✓
- "Resume path unchanged" → Step 6 regression run of reload/replay/select tests. ✓

**Placeholder scan:** No TBD/TODO/placeholder steps; every code and command step shows actual content. ✓

**Type/name consistency:** Uses the existing identifiers `buffersRef`, `setItems`, `activeIdRef`, `closeSession`, `suspendSpawn`, `refreshSpawns` exactly as declared in `App.tsx`. Test selectors (`spawn-kebab-`, `spawn-suspend-`, `spawn-select-`, `spawn-dot-`, `data-role="agent"`, `data-role="user"`, `prompt-input`, `prompt-send`) and helpers (`gotoApp`, `spawnFromMarket`, `rowByName`) match those already used in `spawn-lifecycle.spec.ts`. ✓
