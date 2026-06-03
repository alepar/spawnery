import { describe, it, expect } from "vitest";
import { encodePrompt, encodePermResponse, decodeFrame, type Frame } from "./frames";

describe("frames codec", () => {
  it("encodes a prompt as an ndjson line", () => {
    expect(new TextDecoder().decode(encodePrompt("hi"))).toBe(`{"kind":"prompt","text":"hi"}\n`);
  });
  it("encodes a perm response", () => {
    expect(new TextDecoder().decode(encodePermResponse("p1", true))).toBe(`{"kind":"perm_response","reqId":"p1","allow":true}\n`);
  });
  it("decodes server frames", () => {
    const f = decodeFrame(`{"seq":3,"kind":"agent","text":"hi"}`) as Frame;
    expect(f.seq).toBe(3); expect(f.kind).toBe("agent"); expect(f.text).toBe("hi");
  });
});
