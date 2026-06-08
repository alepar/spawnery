import { render, act } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";

// ─── Fake ReconnectingSocket ──────────────────────────────────────────────────
// Mirrors the TerminalView test's socket-mock pattern: capture the constructed
// instance (and its opts) so a test can drive onOpen/onDown and inspect sends.
let fakeSocketInstance: {
  sent: (string | Uint8Array)[];
  opts: { onOpen: () => void; onDown: () => void; onMessage?: (d: ArrayBuffer | string) => void };
  binaryType: string;
  send: (d: string | Uint8Array) => void;
  close: () => void;
} | null = null;
let socketCtorCount = 0;

vi.mock("@/shell/reconnectingSocket", () => ({
  ReconnectingSocket: vi.fn((_url: string, opts: any) => {
    socketCtorCount++;
    fakeSocketInstance = {
      sent: [],
      opts,
      binaryType: "blob",
      send(d: string | Uint8Array) { this.sent.push(d); },
      close: vi.fn(),
    };
    return fakeSocketInstance;
  }),
}));

import { AcpSessionPanel } from "./AcpSessionPanel";
import { useSessionStore } from "./store";

beforeEach(() => {
  fakeSocketInstance = null;
  socketCtorCount = 0;
  useSessionStore.getState().bindSpawn("__reset__");
});

describe("AcpSessionPanel — gate the socket on session readiness", () => {
  it("does NOT open a socket / send a bind while ready=false, and shows conn=waiting", () => {
    render(<AcpSessionPanel spawnId="s1" sessionId="2" active ready={false} />);
    expect(socketCtorCount).toBe(0);          // no socket constructed
    expect(fakeSocketInstance).toBeNull();    // ⇒ no bind sent into the void
    expect(useSessionStore.getState().conn["2"]).toBe("waiting"); // honest grey-pulse dot
  });

  it("opens the socket and sends the bind once the session flips ready=true", () => {
    const { rerender } = render(<AcpSessionPanel spawnId="s1" sessionId="2" active ready={false} />);
    expect(fakeSocketInstance).toBeNull();

    act(() => { rerender(<AcpSessionPanel spawnId="s1" sessionId="2" active ready={true} />); });
    expect(socketCtorCount).toBe(1);
    expect(fakeSocketInstance).not.toBeNull();
    expect(useSessionStore.getState().conn["2"]).toBe("connecting");

    // onOpen drives the bind + connected state, exactly as before the gate.
    act(() => { fakeSocketInstance!.opts.onOpen(); });
    const bindMsg = fakeSocketInstance!.sent[0];
    expect(typeof bindMsg).toBe("string");
    const bind = JSON.parse(bindMsg as string);
    expect(bind.spawnId).toBe("s1");
    expect(bind.sessionId).toBe("2");
    expect(bind.cursor).toBe(0);
    expect(useSessionStore.getState().conn["2"]).toBe("connected");
  });

  it("opens the socket immediately when mounted ready=true (no regression for the primary)", () => {
    render(<AcpSessionPanel spawnId="s1" sessionId="0" active ready={true} />);
    expect(socketCtorCount).toBe(1);
    expect(fakeSocketInstance).not.toBeNull();
  });
});
