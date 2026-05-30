import { describe, it, expect } from "vitest";
import { Client } from "./client";
import type { WebSocketLike } from "./conn";

// A fake WS that lets the test capture sent messages and inject incoming ones.
class FakeWS implements WebSocketLike {
  binaryType = "arraybuffer";
  onmessage: ((ev: { data: any }) => void) | null = null;
  sent: any[] = [];
  send(data: Uint8Array) {
    this.sent.push(JSON.parse(new TextDecoder().decode(data).trim()));
  }
  inject(m: any) {
    this.onmessage?.({ data: new TextEncoder().encode(JSON.stringify(m) + "\n") });
  }
}

describe("Client", () => {
  it("resolves a call when the matching response arrives", async () => {
    const ws = new FakeWS();
    const c = new Client(ws);
    const p = c.initialize();
    expect(ws.sent[0].method).toBe("initialize");
    ws.inject({ id: ws.sent[0].id, result: {} });
    await p;
  });

  it("streams session/update chunks and resolves prompt", async () => {
    const ws = new FakeWS();
    const c = new Client(ws);
    ws.inject({ id: (c as any).nid + 1, result: { sessionId: "s1" } }); // not yet; do via newSession
    // newSession
    const ns = c.newSession("/app");
    ws.inject({ id: ws.sent.at(-1).id, result: { sessionId: "s1" } });
    await ns;

    const chunks: string[] = [];
    const pr = c.prompt("hi", { onText: (t) => chunks.push(t) });
    const promptId = ws.sent.at(-1).id;
    ws.inject({ method: "session/update", params: { sessionId: "s1", update: { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "ECHO: hi" } } } });
    ws.inject({ id: promptId, result: { stopReason: "end_turn" } });
    await pr;
    expect(chunks.join("")).toContain("ECHO: hi");
  });

  it("answers a permission request via the handler", async () => {
    const ws = new FakeWS();
    const c = new Client(ws);
    const ns = c.newSession("/app");
    ws.inject({ id: ws.sent.at(-1).id, result: { sessionId: "s1" } });
    await ns;

    let asked = false;
    const pr = c.prompt("go", { requestPermission: async () => { asked = true; return true; } });
    const promptId = ws.sent.at(-1).id;
    ws.inject({ id: 999, method: "session/request_permission", params: { sessionId: "s1", options: [{ optionId: "allow", name: "Allow", kind: "allow_once" }] } });
    // the client should have responded to id 999
    await new Promise((r) => setTimeout(r, 0));
    const resp = ws.sent.find((m) => m.id === 999);
    expect(asked).toBe(true);
    expect(resp.result.outcome.outcome).toBe("selected");
    ws.inject({ id: promptId, result: { stopReason: "end_turn" } });
    await pr;
  });
});
