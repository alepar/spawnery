import { describe, it, expect } from "vitest";
import { CostLedger, BudgetExceededError } from "./budget";

describe("CostLedger", () => {
  it("does not throw while under budget", () => {
    const ledger = new CostLedger(1000, 60_000, () => 0);
    ledger.recordTokens(500);
    expect(() => ledger.check()).not.toThrow();
    expect(ledger.remaining()).toBe(500);
  });

  it("throws BudgetExceededError when tokens exceed budget", () => {
    const ledger = new CostLedger(1000, 60_000, () => 0);
    ledger.recordTokens(1001);
    expect(() => ledger.check()).toThrow(BudgetExceededError);
  });

  it("throws BudgetExceededError once the injected clock passes the wall-clock cap", () => {
    let now = 0;
    const ledger = new CostLedger(1000, 60_000, () => now);
    expect(() => ledger.check()).not.toThrow();
    now = 60_001;
    expect(() => ledger.check()).toThrow(BudgetExceededError);
  });
});
