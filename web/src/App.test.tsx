import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import type { SpawnView } from "./api/spawnlet";
import type { SessionDescriptor } from "./api/sessions";
import type { Frame } from "./acp/frames";

// --- Mocks ---------------------------------------------------------------
// The api is fully mocked so no network happens; listSpawns drives the poll + sidebar.
const listSpawnsMock = vi.fn(async (): Promise<SpawnView[]> => []);
const recreateSpawnMock = vi.fn(async (_id: string) => {});
vi.mock("./api/spawnlet", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api/spawnlet")>();
  return {
    ...actual,
    listSpawns: () => listSpawnsMock(),
    createSpawn: vi.fn(async () => "new-id"),
    renameSpawn: vi.fn(async () => {}),
    suspendSpawn: vi.fn(async () => {}),
    resumeSpawn: vi.fn(async () => {}),
    recreateSpawn: (id: string) => recreateSpawnMock(id),
    deleteSpawn: vi.fn(async () => {}),
    listAgentImages: vi.fn(async () => []),
  };
});

// Sessions roster: the active spawn auto-registers session #0. Drive it so SpawnTabs mounts a panel.
const acp0: SessionDescriptor = { sessionId: "0", transport: "acp", runnable: "goose-acp", status: "active", pinned: true };
const mosh0: SessionDescriptor = { sessionId: "0", transport: "mosh", runnable: "shell", status: "active", pinned: true };
const listSessionsMock = vi.fn(async (): Promise<SessionDescriptor[]> => [acp0]);
vi.mock("./api/sessions", async (orig) => {
  const actual = await orig<typeof import("./api/sessions")>();
  return { ...actual, listSessions: () => listSessionsMock(), createSession: vi.fn(async () => {}), closeSession: vi.fn(async () => {}) };
});

// Stub the session panels so the test doesn't depend on real sockets; they record the spawn/session
// they bound so we can assert the active spawn's panel mounted.
const acpPanels: { spawnId: string; sessionId: string }[] = [];
vi.mock("./sessions/AcpSessionPanel", () => ({
  AcpSessionPanel: ({ spawnId, sessionId }: { spawnId: string; sessionId: string }) => { acpPanels.push({ spawnId, sessionId }); return <div>acp</div>; },
}));
const termPanels: { spawnId: string; sessionId: string }[] = [];
vi.mock("./views/TerminalView", () => ({
  TerminalView: ({ spawnId, sessionId }: { spawnId: string; sessionId: string }) => { termPanels.push({ spawnId, sessionId }); return <div>term</div>; },
}));

import { App } from "./App";
import { useSessionStore } from "./sessions/store";

const ACTIVE_SPAWN: SpawnView = { spawnId: "s1", name: "My Spawn", appId: "spawnery/wiki", status: "active", mode: "", model: "", modelApplied: true };

function renderWith(hook: ReturnType<typeof memoryLocation>["hook"]) {
  render(
    <Router hook={hook}>
      <App />
    </Router>,
  );
}

function renderAt(path: string) {
  renderWith(memoryLocation({ path }).hook);
}

beforeEach(() => {
  acpPanels.length = 0;
  termPanels.length = 0;
  useSessionStore.getState().bindSpawn("__reset__");
  recreateSpawnMock.mockClear();
  listSpawnsMock.mockReset();
  listSpawnsMock.mockResolvedValue([ACTIVE_SPAWN]);
  listSessionsMock.mockReset();
  listSessionsMock.mockResolvedValue([acp0]);
  document.title = "";
  // jsdom has no matchMedia; theme.initialTheme() calls it on mount.
  (window as any).matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener: () => {}, removeEventListener: () => {} });
});
afterEach(() => { vi.clearAllTimers(); });

describe("App URL-authoritative nav", () => {
  it("navigating to /spawn/<id> binds that spawn (mounts its session panel)", async () => {
    renderAt("/spawn/s1");
    // The reconciliation effect runs bindSpawn("s1"); SpawnTabs polls the roster and mounts session 0.
    await waitFor(() => expect(acpPanels.some((p) => p.spawnId === "s1")).toBe(true));
  });

  it("does not mount a session panel when nav is a non-spawn section", async () => {
    renderAt("/templates");
    // Give the poll a tick; with no active spawn bound, no SpawnTabs (and so no panel) mounts.
    await waitFor(() => expect(listSpawnsMock).toHaveBeenCalled());
    expect(acpPanels.length).toBe(0);
    expect(termPanels.length).toBe(0);
  });

  it("normalizes / to /templates (replace, not a new history entry)", async () => {
    const mem = memoryLocation({ path: "/", record: true });
    renderWith(mem.hook);
    // The normalize effect replaces "/" with "/templates"; replace means the history did not grow
    // into a "/" then "/templates" push pair — it stays a single entry pointing at /templates.
    await waitFor(() => expect(mem.history[mem.history.length - 1]).toBe("/templates"));
    expect(mem.history).toEqual(["/templates"]);
    await waitFor(() => expect(document.title).toBe("Spawnery — Templates"));
  });

  it("sets document.title per section: templates", async () => {
    renderAt("/templates");
    await waitFor(() => expect(document.title).toBe("Spawnery — Templates"));
  });

  it("sets document.title per section: my-apps / publish / settings / app", async () => {
    renderAt("/my-apps");
    await waitFor(() => expect(document.title).toBe("Spawnery — My Apps"));

    renderAt("/publish");
    await waitFor(() => expect(document.title).toBe("Spawnery — Publish"));

    renderAt("/settings");
    await waitFor(() => expect(document.title).toBe("Spawnery — Settings"));

    renderAt("/templates/spawnery%2Fwiki");
    await waitFor(() => expect(document.title).toBe("Spawnery — spawnery/wiki"));
  });

  it("sets document.title for a spawn from its name", async () => {
    renderAt("/spawn/s1");
    await waitFor(() => expect(document.title).toBe("Spawnery — My Spawn"));
  });

  it("falls back to spawnId in the title when the spawn is unknown", async () => {
    listSpawnsMock.mockResolvedValue([]);
    renderAt("/spawn/ghost");
    await waitFor(() => expect(document.title).toBe("Spawnery — ghost"));
  });

  // A mosh (tmux) session surfaces in a TerminalView panel, fed by the roster's transport. The header
  // dot is now driven by the store's per-session conn (TerminalView -> SpawnTabs.setConn); the live
  // dot behavior itself is covered by SpawnTabs/store tests, so here we just assert the panel mounts.
  it("mounts a terminal panel for a mosh-transport session", async () => {
    listSessionsMock.mockResolvedValue([mosh0]);
    renderAt("/spawn/s1");
    await waitFor(() => expect(termPanels.some((p) => p.spawnId === "s1")).toBe(true));
    expect(acpPanels.some((p) => p.spawnId === "s1")).toBe(false);
  });

  // The sidebar's Recreate item (unreachable/error spawns) calls the RecreateSpawn RPC and lands the
  // user on the recovering spawn's view.
  it("kebab → Recreate calls recreateSpawn and navigates to the spawn", async () => {
    listSpawnsMock.mockResolvedValue([{ ...ACTIVE_SPAWN, status: "unreachable" }]);
    renderAt("/settings");
    await waitFor(() => expect(screen.getByTestId("spawn-kebab-s1")).toBeTruthy());
    await userEvent.click(screen.getByTestId("spawn-kebab-s1"));
    await userEvent.click(screen.getByTestId("spawn-recreate-s1"));
    expect(recreateSpawnMock).toHaveBeenCalledWith("s1");
    // navigate({section:"spawn", spawnId:"s1"}) — the title re-derives off the spawn's name.
    await waitFor(() => expect(document.title).toBe("Spawnery — My Spawn"));
  });

  // §5 regression (spec 2026-06-08 sidebar-lifecycle, corrected §3 amendment): recreating the
  // CURRENTLY-SELECTED spawn must not leave the dead container's transcript around. No explicit
  // rebind happens (navigate-to-same is a no-op); the clear is structural — recreate drops the
  // spawn's session mirror, so ListSessions returns empty and SpawnTabs' roster poll
  // (reconcileRoster) wipes the stale runtime. This guards that poll mechanism.
  it("recreating the selected spawn drops the dead transcript via the roster poll", async () => {
    listSpawnsMock.mockResolvedValue([{ ...ACTIVE_SPAWN, status: "unreachable" }]);
    renderAt("/spawn/s1");
    // Session 0 registers and a (now-dead) transcript exists in the store.
    await waitFor(() => expect(acpPanels.some((p) => p.spawnId === "s1")).toBe(true));
    useSessionStore.getState().applyFrame("0", { kind: "agent", text: "from the dead container" } as Frame);
    expect(useSessionStore.getState().acp["0"].items).toHaveLength(1);

    // Recreate: the RPC drops the session mirror with the spawn's route -> ListSessions is empty.
    listSessionsMock.mockResolvedValue([]);
    await userEvent.click(screen.getByTestId("spawn-kebab-s1"));
    await userEvent.click(screen.getByTestId("spawn-recreate-s1"));
    expect(recreateSpawnMock).toHaveBeenCalledWith("s1");

    // The 3s roster poll reconciles the empty roster and wipes the stale runtime.
    await waitFor(() => expect(useSessionStore.getState().acp["0"]).toBeUndefined(), { timeout: 5000 });
  }, 10000);

  // Browser back/forward drives wouter's location store from OUTSIDE React (a popstate). A single
  // mounted App must react to that external location change: re-render the right section AND re-derive
  // the title. memoryLocation.navigate() emits exactly that out-of-band store update.
  it("reacts to an external location change (back/forward popstate) — title + binding follow the URL", async () => {
    const mem = memoryLocation({ path: "/settings", record: true });
    renderWith(mem.hook);
    await waitFor(() => expect(document.title).toBe("Spawnery — Settings"));
    // No spawn bound on /settings.
    expect(acpPanels.length).toBe(0);

    // Simulate forward to the active spawn (out-of-band, like the browser advancing history).
    mem.navigate("/spawn/s1");
    await waitFor(() => expect(document.title).toBe("Spawnery — My Spawn"));
    // The reconciliation effect re-binds s1 and SpawnTabs mounts its session panel.
    await waitFor(() => expect(acpPanels.some((p) => p.spawnId === "s1")).toBe(true));

    // Simulate back to settings again: the title re-derives off the section.
    mem.navigate("/settings");
    await waitFor(() => expect(document.title).toBe("Spawnery — Settings"));
  });
});
