import { render, act, fireEvent, screen } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";

const authMocks = vi.hoisted(() => ({
  buildBind: vi.fn(async (
    spawnId: string, sessionId: string, clientId: string, cursor: number, attachmentSequence: number,
  ) => ({
    spawnId, sessionId, clientId, cursor, token: "cp-old", nodeAccessToken: "node-old",
    signedIntent: "open-intent",
    authorization: {
      spawnId, sessionId, clientId, attachmentSequence,
      generation: 7n, targetNodeId: "node-1",
    },
  })),
  buildNodeReauth: vi.fn(async (_authorization: unknown, nodeAccessToken: string) => ({
    type: "nodeReauth", nodeAccessToken, signedIntent: "reauth-intent",
  })),
}));

vi.mock("@/auth/sessionBind", () => ({ buildSessionBindFrame: authMocks.buildBind }));
vi.mock("@/auth/sessionReauth", () => ({ buildNodeReauthControl: authMocks.buildNodeReauth }));

// ─── Fake ReconnectingSocket ──────────────────────────────────────────────────
// Mirrors the TerminalView test's socket-mock pattern: capture the constructed
// instance (and its opts) so a test can drive onOpen/onDown and inspect sends.
let fakeSocketInstance: {
  sent: (string | Uint8Array)[];
  opts: { onOpen: () => void | Promise<void>; onDown: () => void; onMessage?: (d: ArrayBuffer | string) => void };
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
  vi.unstubAllEnvs();
  authMocks.buildBind.mockClear();
  authMocks.buildNodeReauth.mockReset();
  authMocks.buildNodeReauth.mockImplementation(async (_authorization: unknown, nodeAccessToken: string) => ({
    type: "nodeReauth", nodeAccessToken, signedIntent: "reauth-intent",
  }));
});

describe("AcpSessionPanel — gate the socket on session readiness", () => {
  it("does NOT open a socket / send a bind while ready=false, and shows conn=waiting", () => {
    render(<AcpSessionPanel spawnId="s1" sessionId="2" active ready={false} />);
    expect(socketCtorCount).toBe(0);          // no socket constructed
    expect(fakeSocketInstance).toBeNull();    // ⇒ no bind sent into the void
    expect(useSessionStore.getState().conn["2"]).toBe("waiting"); // honest grey-pulse dot
  });

  it("opens the socket and sends the bind once the session flips ready=true", async () => {
    const { rerender } = render(<AcpSessionPanel spawnId="s1" sessionId="2" active ready={false} />);
    expect(fakeSocketInstance).toBeNull();

    act(() => { rerender(<AcpSessionPanel spawnId="s1" sessionId="2" active ready={true} />); });
    expect(socketCtorCount).toBe(1);
    expect(fakeSocketInstance).not.toBeNull();
    expect(useSessionStore.getState().conn["2"]).toBe("connecting");

    // onOpen drives the bind + connected state (async: it awaits the session-open intent first).
    await act(async () => { await fakeSocketInstance!.opts.onOpen(); });
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

  it("sends CP and node reauth separately after an atomic pair refresh", async () => {
    vi.stubEnv("VITE_AUTH_ENABLED", "1");
    const { useSessionStore: useAuthStore } = await import("@/auth/session");
    useAuthStore.setState({ cpAccessToken: "cp-old", nodeAccessToken: "node-old", status: "authed" });
    render(<AcpSessionPanel spawnId="s1" sessionId="2" active ready />);
    await act(async () => { await fakeSocketInstance!.opts.onOpen(); });
    fakeSocketInstance!.sent.length = 0;
    act(() => { useAuthStore.setState({ cpAccessToken: "cp-new", nodeAccessToken: "node-new" }); });
    await vi.waitFor(() => expect(authMocks.buildNodeReauth).toHaveBeenCalled());
    const controls = fakeSocketInstance!.sent.map((raw) => JSON.parse(raw as string));
    expect(controls).toContainEqual({ type: "reauth", token: "cp-new" });
    expect(controls).toContainEqual({
      type: "nodeReauth", nodeAccessToken: "node-new", signedIntent: "reauth-intent",
    });
  });

  it("closes the current surface when node reauth construction fails", async () => {
    vi.stubEnv("VITE_AUTH_ENABLED", "1");
    const { useSessionStore: useAuthStore } = await import("@/auth/session");
    useAuthStore.setState({ cpAccessToken: "cp-old", nodeAccessToken: "node-old", status: "authed" });
    authMocks.buildNodeReauth.mockRejectedValueOnce(new Error("key lost"));
    render(<AcpSessionPanel spawnId="s1" sessionId="2" active ready />);
    await act(async () => { await fakeSocketInstance!.opts.onOpen(); });
    act(() => { useAuthStore.setState({ cpAccessToken: "cp-new", nodeAccessToken: "node-new" }); });
    await vi.waitFor(() => expect(fakeSocketInstance!.close).toHaveBeenCalled());
  });
});

// ─── Chat controls + enrichment data (sp-x8y4.2) ─────────────────────────────
// The panel must feed the store's commands/mode into ChatView and wire the upward
// cancel / set_mode control frames through the live socket, mirroring onSend.
const dec = new TextDecoder();
function lastSentFrame(): { kind: string; modeId?: string } {
  const raw = fakeSocketInstance!.sent.at(-1);
  return JSON.parse(dec.decode(raw as Uint8Array));
}

function mountConnected(sessionId: string) {
  const view = render(<AcpSessionPanel spawnId="s1" sessionId={sessionId} active ready={true} />);
  act(() => { fakeSocketInstance!.opts.onOpen(); });
  return view;
}

describe("AcpSessionPanel — chat controls + enrichment data", () => {
  it("StopButton click sends a cancel frame over the socket", () => {
    mountConnected("0");
    // Busy turn -> StopButton renders.
    act(() => { useSessionStore.getState().applyFrame("0", { kind: "turn", state: "busy", queued: 0 }); });
    fireEvent.click(screen.getByTestId("stop-button"));
    expect(lastSentFrame()).toEqual({ kind: "cancel" });
  });

  it("ModeSelector change sends set_mode with the chosen id", () => {
    mountConnected("0");
    act(() => {
      useSessionStore.getState().applyFrame("0", {
        kind: "mode",
        mode: { current: "default", available: [{ id: "default", name: "Default" }, { id: "plan", name: "Plan" }] },
      });
    });
    // mode from the store reached ChatView: the selector renders with the agent's modes.
    const selector = screen.getByTestId("mode-selector");
    expect(selector).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Session mode"), { target: { value: "plan" } });
    expect(lastSentFrame()).toEqual({ kind: "set_mode", modeId: "plan" });
  });

  it("commands from the store reach ChatView (slash menu lists them)", () => {
    mountConnected("0");
    act(() => {
      useSessionStore.getState().applyFrame("0", {
        kind: "commands",
        cmds: [{ name: "compact", description: "Compact the context" }],
      });
    });
    // Typing `/` opens the command menu only when commands reached PromptInput via ChatView.
    fireEvent.change(screen.getByTestId("prompt-input"), { target: { value: "/" } });
    expect(screen.getByTestId("command-menu")).toBeTruthy();
    expect(screen.getByTestId("command-option").textContent).toContain("/compact");
  });
});
