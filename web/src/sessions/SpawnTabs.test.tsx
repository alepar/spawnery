import { render, screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi, beforeEach } from "vitest";
import type { SessionDescriptor } from "@/api/sessions";

const listSessionsMock = vi.fn(async (): Promise<SessionDescriptor[]> => []);
const createSessionMock = vi.fn(async (_spawnId: string, _transport: string, _runnable: string) => {});
const closeSessionMock = vi.fn(async (_spawnId: string, _sessionId: string) => {});
vi.mock("@/api/sessions", async (orig) => {
  const actual = await orig<typeof import("@/api/sessions")>();
  return {
    ...actual,
    listSessions: () => listSessionsMock(),
    createSession: (a: string, b: string, c: string) => createSessionMock(a, b, c),
    closeSession: (a: string, b: string) => closeSessionMock(a, b),
  };
});
vi.mock("@/api/spawnlet", async (orig) => {
  const actual = await orig<typeof import("@/api/spawnlet")>();
  return { ...actual, listAgentImages: vi.fn(async () => [{ image: "img:1", runnables: [{ id: "goose-acp", label: "Goose", mode: "acp" }] }]) };
});
// Panels pull in xterm/sockets — stub both to inert markers.
vi.mock("@/sessions/AcpSessionPanel", () => ({ AcpSessionPanel: ({ sessionId }: { sessionId: string }) => <div>acp-{sessionId}</div> }));
vi.mock("@/views/TerminalView", () => ({ TerminalView: ({ sessionId }: { sessionId: string }) => <div>term-{sessionId}</div> }));

import { SpawnTabs } from "./SpawnTabs";
import { useSessionStore } from "./store";

const acp0: SessionDescriptor = { sessionId: "0", transport: "acp", runnable: "goose-acp", status: "active", pinned: true };
const shell1: SessionDescriptor = { sessionId: "1", transport: "mosh", runnable: "shell", status: "active", pinned: false };

beforeEach(() => {
  useSessionStore.getState().bindSpawn("__reset__");
  listSessionsMock.mockReset(); listSessionsMock.mockResolvedValue([acp0]);
  createSessionMock.mockReset(); createSessionMock.mockResolvedValue(undefined);
  closeSessionMock.mockReset(); closeSessionMock.mockResolvedValue(undefined);
});

describe("SpawnTabs", () => {
  it("renders a tab per session from the roster; session #0 is pinned (no close button)", async () => {
    listSessionsMock.mockResolvedValue([acp0, shell1]);
    render(<SpawnTabs spawnId="s1" />);
    await waitFor(() => screen.getByTestId("tab-0"));
    expect(screen.getByTestId("tab-1")).toBeInTheDocument();
    expect(screen.queryByTestId("close-0")).toBeNull();        // pinned
    expect(screen.getByTestId("close-1")).toBeInTheDocument(); // closeable
  });

  it("keeps all panels mounted; only the active one is visible", async () => {
    listSessionsMock.mockResolvedValue([acp0, shell1]);
    render(<SpawnTabs spawnId="s1" />);
    await waitFor(() => screen.getByTestId("panel-0"));
    expect(screen.getByTestId("panel-1")).toBeInTheDocument();      // mounted
    expect(screen.getByTestId("panel-0")).toHaveStyle({ display: "block" });
    expect(screen.getByTestId("panel-1")).toHaveStyle({ display: "none" });
  });

  it("the + menu lists image runnables + shell and CreateSession opens the right transport", async () => {
    render(<SpawnTabs spawnId="s1" />);
    await waitFor(() => screen.getByTestId("add-session"));
    await userEvent.click(screen.getByTestId("add-session"));
    expect(screen.getByTestId("new-session-goose-acp")).toBeInTheDocument();
    expect(screen.getByTestId("new-session-shell")).toBeInTheDocument();
    listSessionsMock.mockResolvedValue([acp0, shell1]); // after create the roster has the shell
    await userEvent.click(screen.getByTestId("new-session-shell"));
    expect(createSessionMock).toHaveBeenCalledWith("s1", "mosh", "shell");
    await waitFor(() => expect(useSessionStore.getState().activeId).toBe("1")); // auto-activate the new tab
  });

  it("closing a tab calls CloseSession and falls back to session #0", async () => {
    listSessionsMock.mockResolvedValue([acp0, shell1]);
    render(<SpawnTabs spawnId="s1" />);
    await waitFor(() => screen.getByTestId("close-1"));
    act(() => useSessionStore.getState().setActive("1"));
    listSessionsMock.mockResolvedValue([acp0]); // 1 reaped
    await userEvent.click(screen.getByTestId("close-1"));
    expect(closeSessionMock).toHaveBeenCalledWith("s1", "1");
    await waitFor(() => expect(useSessionStore.getState().activeId).toBe("0"));
  });
});
