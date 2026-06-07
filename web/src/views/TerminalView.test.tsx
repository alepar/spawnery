import { render } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";

// jsdom doesn't implement ResizeObserver — stub it so TerminalView can mount.
globalThis.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
};

// ─── Fake xterm Terminal ─────────────────────────────────────────────────────
// Captures onData/onResize callbacks and write calls without a real DOM/canvas.
let capturedOnData: ((d: string) => void) | null = null;
let writtenData: (string | Uint8Array)[] = [];
let capturedWriteCallbacks: (() => void)[] = [];

const mockTerminal = {
  loadAddon: vi.fn(),
  open: vi.fn(),
  write: vi.fn((d: string | Uint8Array, cb?: () => void) => { writtenData.push(d); if (cb) capturedWriteCallbacks.push(cb); }),
  onData: vi.fn((cb: (d: string) => void) => { capturedOnData = cb; return { dispose: vi.fn() }; }),
  onResize: vi.fn(() => ({ dispose: vi.fn() })),
  cols: 80,
  rows: 24,
  dispose: vi.fn(),
};

const mockFitAddon = {
  fit: vi.fn(),
  dispose: vi.fn(),
};

vi.mock("@xterm/xterm", () => ({
  Terminal: vi.fn(() => mockTerminal),
}));
vi.mock("@xterm/addon-fit", () => ({
  FitAddon: vi.fn(() => mockFitAddon),
}));
vi.mock("@xterm/xterm/css/xterm.css", () => ({}));

// ─── Fake ReconnectingSocket ──────────────────────────────────────────────────
let fakeSocketInstance: {
  sent: (string | Uint8Array)[] ;
  opts: { onOpen: () => void; onDown: () => void; onMessage?: (d: ArrayBuffer | string) => void };
  binaryType: string;
  send: (d: string | Uint8Array) => void;
  close: () => void;
} | null = null;

vi.mock("@/shell/reconnectingSocket", () => ({
  ReconnectingSocket: vi.fn((_url: string, opts: any) => {
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

// ─── Tests ───────────────────────────────────────────────────────────────────
import { TerminalView } from "./TerminalView";

beforeEach(() => {
  capturedOnData = null;
  writtenData = [];
  capturedWriteCallbacks = [];
  fakeSocketInstance = null;
  mockTerminal.write.mockClear();
  mockTerminal.open.mockClear();
  mockTerminal.onData.mockClear();
  mockTerminal.onResize.mockClear();
  mockTerminal.loadAddon.mockClear();
  mockTerminal.dispose.mockClear();
  mockFitAddon.fit.mockClear();
});

describe("TerminalView", () => {
  it("renders a div with data-testid=terminal-view", () => {
    const { getByTestId } = render(<TerminalView spawnId="sp1" />);
    expect(getByTestId("terminal-view")).toBeTruthy();
  });

  it("sends a JSON bind with spawnId on socket open", () => {
    render(<TerminalView spawnId="sp1" />);
    expect(fakeSocketInstance).not.toBeNull();
    // Trigger the onOpen callback
    fakeSocketInstance!.opts.onOpen();
    // First send should be the JSON bind
    const bindMsg = fakeSocketInstance!.sent[0];
    expect(typeof bindMsg).toBe("string");
    const bind = JSON.parse(bindMsg as string);
    expect(bind.spawnId).toBe("sp1");
    expect(typeof bind.clientId).toBe("string");
    expect(bind.cursor).toBe(0);
  });

  it("writes received ArrayBuffer bytes to the terminal", () => {
    render(<TerminalView spawnId="sp1" />);
    expect(fakeSocketInstance).not.toBeNull();
    // Simulate receiving binary data from server
    const data = new Uint8Array([72, 101, 108, 108, 111]); // "Hello"
    fakeSocketInstance!.opts.onMessage!(data.buffer);
    expect(writtenData.length).toBe(1);
    // terminal.write receives a Uint8Array view over the ArrayBuffer
    expect(writtenData[0] instanceof Uint8Array).toBe(true);
  });

  it("sends a 0x00-prefixed input frame when terminal onData fires", () => {
    render(<TerminalView spawnId="sp1" />);
    expect(fakeSocketInstance).not.toBeNull();
    expect(capturedOnData).not.toBeNull();
    // Trigger onOpen so the socket is ready
    fakeSocketInstance!.opts.onOpen();
    const sentBefore = fakeSocketInstance!.sent.length;
    // Simulate user typing "x"
    capturedOnData!("x");
    const frames = fakeSocketInstance!.sent.slice(sentBefore);
    // Should have sent one frame
    expect(frames.length).toBe(1);
    const frame = frames[0] as Uint8Array;
    expect(frame instanceof Uint8Array).toBe(true);
    expect(frame[0]).toBe(0x00); // TMUX_OP_INPUT
    expect(new TextDecoder().decode(frame.slice(1))).toBe("x");
  });

  it("writes received string data to the terminal as Uint8Array bytes", () => {
    render(<TerminalView spawnId="sp1" />);
    fakeSocketInstance!.opts.onMessage!("hello\r\n");
    // TextEncoder in jsdom runs in a different realm so instanceof Uint8Array is unreliable;
    // ArrayBuffer.isView works across realms and confirms the value is a typed array view.
    expect(ArrayBuffer.isView(writtenData[0])).toBe(true);
    expect(new TextDecoder().decode(writtenData[0] as Uint8Array)).toBe("hello\r\n");
  });

  it("observes a wedge via console.warn when the backlog threshold is exceeded and xterm stalls", () => {
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
    // Use a tiny threshold (10 bytes) so a small burst triggers a wedge.
    // The mock term.write captures callbacks but never invokes them, simulating a stalled xterm.
    render(<TerminalView spawnId="sp-wedge" backlogThreshold={10} />);
    expect(fakeSocketInstance).not.toBeNull();
    // Send two 6-byte messages; after the second the outstanding backlog is 12 > 10 → wedge.
    fakeSocketInstance!.opts.onMessage!(new Uint8Array(6).buffer);
    fakeSocketInstance!.opts.onMessage!(new Uint8Array(6).buffer);
    expect(warnSpy).toHaveBeenCalledOnce();
    expect(warnSpy.mock.calls[0][0]).toMatch(/terminal backlog wedge/);
    warnSpy.mockRestore();
  });
});
