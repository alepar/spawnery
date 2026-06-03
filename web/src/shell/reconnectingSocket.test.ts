import { describe, it, expect, vi } from "vitest";
import { ReconnectingSocket, SCHEDULE, type PartySocketLike, type MakeSocket } from "./reconnectingSocket";

// A controllable fake standing in for a PartySocket instance.
class FakeSocket implements PartySocketLike {
  binaryType = "blob";
  onmessage: ((ev: { data: any }) => void) | null = null;
  closed = false;
  _options: { connectionTimeout?: number };
  sent: any[] = [];
  private listeners: Record<string, ((ev: any) => void)[]> = { open: [], close: [], message: [] };
  constructor(public url: string, public options: any) {
    this._options = { connectionTimeout: options.connectionTimeout };
  }
  send(data: any) { this.sent.push(data); }
  close() { this.closed = true; }
  addEventListener(type: "open" | "close" | "message", cb: (ev: any) => void) { this.listeners[type].push(cb); }
  emit(type: "open" | "close" | "message", ev?: any) { this.listeners[type].forEach((cb) => cb(ev)); }
}

function setup() {
  let fake: FakeSocket | undefined;
  const make: MakeSocket = (url, options) => (fake = new FakeSocket(url, options));
  const onOpen = vi.fn();
  const onDown = vi.fn();
  const rs = new ReconnectingSocket("ws://x/ws", { onOpen, onDown, makeSocket: make });
  return { rs, fake: fake!, onOpen, onDown };
}

describe("ReconnectingSocket", () => {
  it("starts with the first schedule step and Infinity retries", () => {
    const { fake } = setup();
    expect(fake._options.connectionTimeout).toBe(SCHEDULE[0]); // 500
    expect(fake.options.maxRetries).toBe(Infinity);
  });

  it("escalates connectionTimeout on each close, capping at the last step", () => {
    const { fake, onDown } = setup();
    const got: number[] = [];
    for (let i = 0; i < SCHEDULE.length + 1; i++) {
      fake.emit("close");
      got.push(fake._options.connectionTimeout!);
    }
    expect(got).toEqual([2500, 5000, 15000, 30000, 30000, 30000]);
    expect(onDown).toHaveBeenCalledTimes(SCHEDULE.length + 1);
  });

  it("resets to the first step on open (success resets)", () => {
    const { fake, onOpen } = setup();
    fake.emit("close"); // -> 2500
    fake.emit("close"); // -> 5000
    fake.emit("open");
    expect(fake._options.connectionTimeout).toBe(SCHEDULE[0]); // 500
    expect(onOpen).toHaveBeenCalledTimes(1);
    fake.emit("close"); // first failure of the new chain -> 2500 again
    expect(fake._options.connectionTimeout).toBe(2500);
  });

  it("forwards messages to onmessage and sends through the socket", () => {
    const { rs, fake } = setup();
    const got: any[] = [];
    rs.onmessage = (ev) => got.push(ev.data);
    fake.emit("message", { data: "hello" });
    expect(got).toEqual(["hello"]);
    rs.send("bind");
    expect(fake.sent).toEqual(["bind"]);
  });

  it("close() stops reconnection", () => {
    const { rs, fake } = setup();
    rs.close();
    expect(fake.closed).toBe(true);
  });

  it("forwards binaryType get/set to the socket", () => {
    const { rs, fake } = setup();
    rs.binaryType = "arraybuffer";
    expect(fake.binaryType).toBe("arraybuffer");
    expect(rs.binaryType).toBe("arraybuffer");
  });
});

describe("partysocket structural guard", () => {
  it("real PartySocket exposes _options.connectionTimeout (pinned-version contract)", async () => {
    const { WebSocket: PartySocket } = await import("partysocket");
    const ps: any = new PartySocket("ws://localhost:1", [], { connectionTimeout: 500, startClosed: true });
    expect(typeof ps._options.connectionTimeout).toBe("number");
    ps.close();
  });
});
