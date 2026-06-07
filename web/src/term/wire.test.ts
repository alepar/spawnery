import { describe, it, expect } from "vitest";
import { encodeInput, encodeResize, TMUX_OP_INPUT, TMUX_OP_RESIZE } from "./wire";

describe("term wire", () => {
  it("encodes input with the 0x00 opcode", () => {
    const f = encodeInput("hi");
    expect(f[0]).toBe(TMUX_OP_INPUT);
    expect(new TextDecoder().decode(f.slice(1))).toBe("hi");
  });
  it("encodes resize with the 0x01 opcode + ASCII 'cols rows'", () => {
    const f = encodeResize(120, 40);
    expect(f[0]).toBe(TMUX_OP_RESIZE);
    expect(new TextDecoder().decode(f.slice(1))).toBe("120 40");
  });
});
