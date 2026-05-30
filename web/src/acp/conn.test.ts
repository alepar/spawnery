import { describe, it, expect } from "vitest";
import { Conn } from "./conn";
import type { Message } from "./types";

// Minimal WebSocket-like fake; we drive onmessage manually.
class FakeWS {
  binaryType = "arraybuffer";
  onmessage: ((ev: { data: any }) => void) | null = null;
  sent: Uint8Array[] = [];
  send(data: Uint8Array) { this.sent.push(data); }
  feed(s: string) { this.onmessage?.({ data: new TextEncoder().encode(s) }); }
}

describe("Conn", () => {
  it("splits ndjson across chunk boundaries", () => {
    const ws = new FakeWS();
    const got: Message[] = [];
    const c = new Conn(ws as any, (m) => got.push(m));
    ws.feed('{"jsonrpc":"2.0","id":1,"result":{}}\n{"method":"sessio');
    ws.feed('n/update","params":{"x":1}}\n');
    expect(got.length).toBe(2);
    expect(got[0].id).toBe(1);
    expect(got[1].method).toBe("session/update");
    c.send({ id: 2, method: "ping" });
    const sentText = new TextDecoder().decode(ws.sent[0]);
    expect(sentText).toBe('{"jsonrpc":"2.0","id":2,"method":"ping"}\n');
  });
});
