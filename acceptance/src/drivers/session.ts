/**
 * SessionPage: a Playwright page-object for the SPA's chat/session view at `/spawn/<id>`
 * (web/src/views/chat/{MessageList,PromptInput}.tsx, web/src/views/ChatView.tsx). Used by the
 * Phase-2 `@agent` scenarios to drive a prompt and assert the RENDERED transcript — never agent
 * prose (design §Assertion strategy) — plus turn-lifecycle signals (working indicator, usage
 * badge, non-normal end).
 *
 * Like drivers/web.ts, this is Playwright-`Page`-driven, so it is type-checked + live-exercised
 * only — no vitest unit file (matches the existing convention that browser page-objects aren't
 * unit-tested).
 */

import type { Locator, Page } from "@playwright/test";
import type { SpawnId } from "./types";

export class SessionPage {
  constructor(private readonly page: Page) {}

  /** open navigates to the spawn's chat view and waits for the prompt box to be ready. */
  async open(spawnId: SpawnId): Promise<void> {
    await this.page.goto(`/spawn/${encodeURIComponent(spawnId)}`);
    await this.page.getByTestId("prompt-input").waitFor({ state: "visible" });
  }

  /**
   * sendPrompt fills the prompt box and presses Enter to send (Shift+Enter would insert a
   * newline instead — PromptInput.tsx). A leading `/` opens the command-menu autocomplete
   * instead of sending as prose, so prompts must not start with one.
   */
  async sendPrompt(text: string): Promise<void> {
    if (text.startsWith("/")) {
      throw new Error(`SessionPage.sendPrompt: prompt must not start with "/" (opens the command menu instead of sending): ${text}`);
    }
    const input = this.page.getByTestId("prompt-input");
    await input.fill(text);
    await input.press("Enter");
  }

  /**
   * waitTurnSettled waits for the turn to finish: the `working-indicator` footer attaching then
   * detaching, when we manage to catch it mid-flight (best-effort — a turn faster than our short
   * probe window never shows it). Note: `waitFor({state:"detached"})` on an element that was
   * NEVER attached resolves immediately (vacuously "not present") — so if the probe misses it, we
   * must NOT race a detach-wait against it (that would report "settled" before the agent has
   * even started). Instead we fall back to waiting directly for a rendered agent row. Fails fast
   * (rather than timing out silently) if the turn ends for a non-normal reason
   * (`turn-ended-indicator`, e.g. "stopped: max tokens").
   */
  async waitTurnSettled(opts: { timeoutMs?: number } = {}): Promise<void> {
    const timeoutMs = opts.timeoutMs ?? 120_000;
    const deadline = Date.now() + timeoutMs;
    const working = this.page.getByTestId("working-indicator");
    const remaining = () => Math.max(1, deadline - Date.now());

    const sawWorking = await working
      .waitFor({ state: "attached", timeout: Math.min(5_000, timeoutMs) })
      .then(() => true)
      .catch(() => false);

    if (sawWorking) {
      await working.waitFor({ state: "detached", timeout: remaining() });
    } else {
      await this.page.locator('[data-role="agent"]').first().waitFor({ state: "attached", timeout: remaining() });
    }

    const ended = this.page.getByTestId("turn-ended-indicator");
    if (await ended.count()) {
      const label = (await ended.first().textContent())?.trim() ?? "";
      if (/^error:/.test(label)) {
        throw new Error(`SessionPage.waitTurnSettled: turn ended with an error: ${label}`);
      }
    }
  }

  /** userMessages locates every rendered user-turn row ([data-role="user"]), oldest first. */
  userMessages(): Locator {
    return this.page.locator('[data-role="user"]');
  }

  /** agentMessages locates every rendered agent-turn row ([data-role="agent"]), oldest first. */
  agentMessages(): Locator {
    return this.page.locator('[data-role="agent"]');
  }

  /**
   * transcriptText concatenates the innerText of every rendered turn row — used STRUCTURALLY
   * (e.g. a reload cross-check that prior rows re-render), never to assert on agent prose.
   */
  async transcriptText(): Promise<string> {
    return this.page.locator('[data-role]').allInnerTexts().then((texts) => texts.join("\n"));
  }

  /** usageBadgeText returns the last turn-usage-badge's text, or null if the agent reported no usage. */
  async usageBadgeText(): Promise<string | null> {
    const badge = this.page.getByTestId("turn-usage-badge");
    if ((await badge.count()) === 0) return null;
    return badge.last().textContent();
  }
}
