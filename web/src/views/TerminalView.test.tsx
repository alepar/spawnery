import { render, waitFor, act } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { useEffect } from "react";

// jsdom doesn't implement ResizeObserver — stub it so TerminalView can mount.
globalThis.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
};

// TerminalView's live-update effect calls document.fonts.load — stub it in jsdom.
(document as unknown as { fonts: { load: ReturnType<typeof vi.fn> } }).fonts = {
  load: vi.fn().mockResolvedValue([]),
};

// ─── Fake xterm Terminal ─────────────────────────────────────────────────────
// Captures onData/onResize callbacks and write calls without a real DOM/canvas.
let capturedOnData: ((d: string) => void) | null = null;
let writtenData: (string | Uint8Array)[] = [];
let capturedWriteCallbacks: (() => void)[] = [];

const mockTerminal = {
  loadAddon: vi.fn(),
  open: vi.fn(),
  focus: vi.fn(),
  refresh: vi.fn(),
  options: {} as { theme?: unknown; fontFamily?: string; fontSize?: number },
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
import { Terminal } from "@xterm/xterm";
import { TerminalView } from "./TerminalView";
import {
  TermSettingsProvider,
  useTermSettings,
  DEFAULT_TERM_SETTINGS,
  resolveActiveTheme,
} from "@/term/settings";
import { fontById } from "@/term/fonts";

const TerminalMock = Terminal as unknown as ReturnType<typeof vi.fn>;

// Renders TerminalView inside the settings provider (required by useTermSettings).
function renderWithSettings(ui: React.ReactElement) {
  return render(<TermSettingsProvider>{ui}</TermSettingsProvider>);
}

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
  mockTerminal.focus.mockClear();
  mockTerminal.refresh.mockClear();
  mockTerminal.options = {};
  mockFitAddon.fit.mockClear();
  TerminalMock.mockClear();
  localStorage.clear();
  document.documentElement.classList.remove("dark");
});

describe("TerminalView", () => {
  it("renders a div with data-testid=terminal-view", () => {
    const { getByTestId } = renderWithSettings(<TerminalView spawnId="sp1" />);
    expect(getByTestId("terminal-view")).toBeTruthy();
  });

  it("focuses the terminal on mount and on spawn switch (so keystrokes go straight into the TUI)", () => {
    const { rerender } = renderWithSettings(<TerminalView spawnId="sp1" />);
    expect(mockTerminal.focus).toHaveBeenCalledTimes(1);
    // Switching spawns re-runs the spawnId-keyed effect (new Terminal) and refocuses.
    rerender(<TermSettingsProvider><TerminalView spawnId="sp2" /></TermSettingsProvider>);
    expect(mockTerminal.focus).toHaveBeenCalledTimes(2);
  });

  it("sends a JSON bind with spawnId on socket open", () => {
    renderWithSettings(<TerminalView spawnId="sp1" />);
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
    renderWithSettings(<TerminalView spawnId="sp1" />);
    expect(fakeSocketInstance).not.toBeNull();
    // Simulate receiving binary data from server
    const data = new Uint8Array([72, 101, 108, 108, 111]); // "Hello"
    fakeSocketInstance!.opts.onMessage!(data.buffer);
    expect(writtenData.length).toBe(1);
    // terminal.write receives a Uint8Array view over the ArrayBuffer
    expect(writtenData[0] instanceof Uint8Array).toBe(true);
  });

  it("sends a 0x00-prefixed input frame when terminal onData fires", () => {
    renderWithSettings(<TerminalView spawnId="sp1" />);
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
    renderWithSettings(<TerminalView spawnId="sp1" />);
    fakeSocketInstance!.opts.onMessage!("hello\r\n");
    // TextEncoder in jsdom runs in a different realm so instanceof Uint8Array is unreliable;
    // ArrayBuffer.isView works across realms and confirms the value is a typed array view.
    expect(ArrayBuffer.isView(writtenData[0])).toBe(true);
    expect(new TextDecoder().decode(writtenData[0] as Uint8Array)).toBe("hello\r\n");
  });

  it("reports socket state via onConn: connecting on mount, connected on open, reconnecting on down", () => {
    const onConn = vi.fn();
    renderWithSettings(<TerminalView spawnId="sp1" onConn={onConn} />);
    expect(fakeSocketInstance).not.toBeNull();
    // onConn("connecting") fires synchronously when the socket is created (before open).
    expect(onConn).toHaveBeenCalledWith("connecting");
    onConn.mockClear();
    fakeSocketInstance!.opts.onOpen();
    expect(onConn).toHaveBeenCalledWith("connected");
    onConn.mockClear();
    fakeSocketInstance!.opts.onDown();
    expect(onConn).toHaveBeenCalledWith("reconnecting");
  });

  it("includes sessionId in the bind frame", () => {
    renderWithSettings(<TerminalView spawnId="s1" sessionId="2" />);
    expect(fakeSocketInstance).not.toBeNull();
    fakeSocketInstance!.opts.onOpen();
    const bindMsg = fakeSocketInstance!.sent[0] as string;
    const bind = JSON.parse(bindMsg);
    expect(bind.sessionId).toBe("2");
    expect(bind.spawnId).toBe("s1");
  });

  it("refits when a hidden panel becomes active (and not while hidden)", async () => {
    // jsdom doesn't do layout, so offsetParent is always null — fake it to "visible" so the
    // visibility-guarded fit() runs. The guard is what blocks fit() while display:none.
    const { rerender, getByTestId } = renderWithSettings(<TerminalView spawnId="s1" sessionId="2" active={false} />);
    const host = getByTestId("terminal-view");
    // Inactive: offsetParent stays null (hidden) -> no fit on mount.
    const before = mockFitAddon.fit.mock.calls.length;
    Object.defineProperty(host, "offsetParent", { get: () => document.body, configurable: true });
    rerender(<TermSettingsProvider><TerminalView spawnId="s1" sessionId="2" active={true} /></TermSettingsProvider>);
    await waitFor(() => expect(mockFitAddon.fit.mock.calls.length).toBeGreaterThan(before));
  });

  it("never fits while hidden (offsetParent null) on re-activation", async () => {
    const { rerender } = renderWithSettings(<TerminalView spawnId="s1" sessionId="2" active={false} />);
    const before = mockFitAddon.fit.mock.calls.length;
    // offsetParent remains null (jsdom default == hidden); activating must NOT fit.
    rerender(<TermSettingsProvider><TerminalView spawnId="s1" sessionId="2" active={true} /></TermSettingsProvider>);
    await new Promise((r) => requestAnimationFrame(() => r(null)));
    expect(mockFitAddon.fit.mock.calls.length).toBe(before);
  });

  it("observes a wedge via console.warn when the backlog threshold is exceeded and xterm stalls", () => {
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
    // Use a tiny threshold (10 bytes) so a small burst triggers a wedge.
    // The mock term.write captures callbacks but never invokes them, simulating a stalled xterm.
    renderWithSettings(<TerminalView spawnId="sp-wedge" backlogThreshold={10} />);
    expect(fakeSocketInstance).not.toBeNull();
    // Send two 6-byte messages; after the second the outstanding backlog is 12 > 10 → wedge.
    fakeSocketInstance!.opts.onMessage!(new Uint8Array(6).buffer);
    fakeSocketInstance!.opts.onMessage!(new Uint8Array(6).buffer);
    expect(warnSpy).toHaveBeenCalledOnce();
    expect(warnSpy.mock.calls[0][0]).toMatch(/terminal backlog wedge/);
    warnSpy.mockRestore();
  });

  // ─── Appearance settings ───────────────────────────────────────────────────
  it("applies the default appearance settings to the terminal on mount", () => {
    renderWithSettings(<TerminalView spawnId="sp1" />);
    // theme/font/size come from DEFAULT_TERM_SETTINGS (app is light by default here).
    expect(mockTerminal.options.fontSize).toBe(DEFAULT_TERM_SETTINGS.fontSize);
    expect(mockTerminal.options.fontFamily).toBe(fontById(DEFAULT_TERM_SETTINGS.fontFamily).stack);
    expect(mockTerminal.options.theme).toEqual(resolveActiveTheme(DEFAULT_TERM_SETTINGS, false));
  });

  it("updates the SAME terminal in place when settings change (no recreate) and re-sends a resize", async () => {
    // Harness exposing the provider's update() so a test can drive a settings change.
    let triggerUpdate: () => void = () => {};
    function Harness() {
      const { update } = useTermSettings();
      useEffect(() => {
        triggerUpdate = () => update({ fontFamily: "fira-code", fontSize: 18 });
      }, [update]);
      return <TerminalView spawnId="sp1" />;
    }
    render(<TermSettingsProvider><Harness /></TermSettingsProvider>);

    // Terminal constructed exactly once on mount.
    expect(TerminalMock).toHaveBeenCalledTimes(1);
    fakeSocketInstance!.opts.onOpen();
    const sentBefore = fakeSocketInstance!.sent.length;

    await act(async () => {
      triggerUpdate();
      // Let the [settings,appDark] effect's document.fonts.load promise resolve.
      await Promise.resolve();
      await Promise.resolve();
    });

    // No recreate: the constructor was not called again — same instance mutated.
    expect(TerminalMock).toHaveBeenCalledTimes(1);
    expect(mockTerminal.options.fontFamily).toBe(fontById("fira-code").stack);
    expect(mockTerminal.options.fontSize).toBe(18);
    expect(mockTerminal.refresh).toHaveBeenCalled();

    // A resize frame was re-sent so the PTY follows the new cell metrics.
    const after = fakeSocketInstance!.sent.slice(sentBefore);
    const resizeFrames = after.filter((f) => f instanceof Uint8Array);
    expect(resizeFrames.length).toBeGreaterThanOrEqual(1);
  });
});
