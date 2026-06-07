import { describe, it, expect } from "vitest";
import { encodePrompt, encodePermResponse, encodeCancel, encodeSetMode, decodeFrame } from "./frames";

describe("frames codec", () => {
  it("keeps existing encoders byte-stable", () => {
    expect(new TextDecoder().decode(encodePrompt("hi"))).toBe(`{"kind":"prompt","text":"hi"}\n`);
    expect(new TextDecoder().decode(encodePermResponse("p1", "allow_once"))).toBe(`{"kind":"perm_response","reqId":"p1","optionId":"allow_once"}\n`);
  });
  it("encodes cancel and set_mode control frames", () => {
    expect(new TextDecoder().decode(encodeCancel())).toBe(`{"kind":"cancel"}\n`);
    expect(new TextDecoder().decode(encodeSetMode("ask"))).toBe(`{"kind":"set_mode","modeId":"ask"}\n`);
  });
  it("decodes a plain server frame", () => {
    const f = decodeFrame(`{"seq":3,"kind":"agent","text":"hi"}`);
    expect(f.seq).toBe(3);
    if (f.kind !== "agent") throw new Error("expected agent");
    expect(f.text).toBe("hi");
  });
  it("decodes a tool frame with a typed payload", () => {
    const f = decodeFrame(`{"seq":6,"kind":"tool","toolId":"t2","tool":{"content":[{"type":"text","text":"out"}],"diff":{"path":"a.go","oldText":"x","newText":"y"},"rawInput":{"a":1}}}`);
    if (f.kind !== "tool") throw new Error("expected tool");
    expect(f.tool?.content?.[0]?.text).toBe("out");
    expect(f.tool?.diff?.path).toBe("a.go");
  });
  it("decodes turn usage/reason/error", () => {
    const f = decodeFrame(`{"seq":8,"kind":"turn","state":"idle","reason":"end_turn","usage":{"input":10,"output":20,"total":30,"cost":0.01},"error":{"code":1,"message":"boom"}}`);
    if (f.kind !== "turn") throw new Error("expected turn");
    expect(f.usage?.total).toBe(30);
    expect(f.reason).toBe("end_turn");
    expect(f.error?.message).toBe("boom");
  });
  it("decodes plan / commands / mode payloads", () => {
    const plan = decodeFrame(`{"seq":7,"kind":"plan","plan":[{"content":"step","status":"pending"}]}`);
    if (plan.kind !== "plan") throw new Error("expected plan");
    expect(plan.plan?.[0]?.content).toBe("step");
    const cmds = decodeFrame(`{"seq":10,"kind":"commands","cmds":[{"name":"/test"}]}`);
    if (cmds.kind !== "commands") throw new Error("expected commands");
    expect(cmds.cmds?.[0]?.name).toBe("/test");
    const mode = decodeFrame(`{"seq":11,"kind":"mode","mode":{"current":"code","available":[{"id":"code","name":"Code"}]}}`);
    if (mode.kind !== "mode") throw new Error("expected mode");
    expect(mode.mode?.current).toBe("code");
  });
  it("decodes perm_request option kinds and perm_response optionId", () => {
    const req = decodeFrame(`{"kind":"perm_request","reqId":"p2","title":"edit?","options":[{"optionId":"allow_once","name":"Allow once","kind":"allow_once"}]}`);
    if (req.kind !== "perm_request") throw new Error("expected perm_request");
    expect(req.options?.[0]?.optionId).toBe("allow_once");
    const resp = decodeFrame(`{"kind":"perm_response","reqId":"p2","optionId":"allow_once"}`);
    if (resp.kind !== "perm_response") throw new Error("expected perm_response");
    expect(resp.optionId).toBe("allow_once");
  });
});
