import { describe, it, expect } from "vitest";
import { BacklogTracker } from "./backlog";

describe("BacklogTracker", () => {
  it("counts a wedge on each rising-edge threshold crossing", () => {
    const bt = new BacklogTracker(100);
    bt.onWrite(60); expect(bt.wedges).toBe(0);   // outstanding 60
    bt.onWrite(60); expect(bt.wedges).toBe(1);   // outstanding 120 → crossed 100 (rising edge)
    bt.onWrite(60); expect(bt.wedges).toBe(1);   // outstanding 180 → still over, no new edge
    bt.onAck(150);  expect(bt.outstanding).toBe(30); // drained below threshold
    bt.onWrite(90); expect(bt.wedges).toBe(2);   // outstanding 120 → crossed again (new rising edge)
  });
  it("never goes negative on over-ack", () => {
    const bt = new BacklogTracker(100);
    bt.onWrite(10); bt.onAck(999); expect(bt.outstanding).toBe(0);
  });
});
