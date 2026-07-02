/**
 * CostLedger: per-run token budget + wall-clock cap with a kill-switch (design §NFRs, cost
 * ceiling) — defence against a cron run silently burning the OpenRouter budget. Phase 0 has no
 * @agent scenarios yet, so nothing feeds tokens; the ledger exists here, unit-proven, for the
 * agent-exercising phases to consume.
 */

export class BudgetExceededError extends Error {
  constructor(reason: string) {
    super(`acceptance run budget exceeded: ${reason}`);
    this.name = "BudgetExceededError";
  }
}

export class CostLedger {
  private tokensUsed = 0;
  private readonly startedAt: number;

  constructor(
    private readonly tokenBudget: number,
    private readonly wallclockMs: number,
    private readonly now: () => number = () => Date.now(),
  ) {
    this.startedAt = this.now();
  }

  /** recordTokens adds n tokens to the running total (does not itself throw — call check()). */
  recordTokens(n: number): void {
    this.tokensUsed += n;
  }

  /** remaining tokens left under budget (can go negative once exceeded). */
  remaining(): number {
    return this.tokenBudget - this.tokensUsed;
  }

  /** check throws BudgetExceededError once the token budget or wall-clock cap is exceeded. */
  check(): void {
    if (this.tokensUsed > this.tokenBudget) {
      throw new BudgetExceededError(`${this.tokensUsed} tokens used > budget ${this.tokenBudget}`);
    }
    const elapsed = this.now() - this.startedAt;
    if (elapsed > this.wallclockMs) {
      throw new BudgetExceededError(`${elapsed}ms elapsed > wall-clock cap ${this.wallclockMs}ms`);
    }
  }
}
