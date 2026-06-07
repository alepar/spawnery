import { render, waitFor } from "@testing-library/react";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import type { SpawnView } from "./api/spawnlet";

// --- Mocks ---------------------------------------------------------------
// The api is fully mocked so no network happens; listSpawns drives the poll + sidebar.
const listSpawnsMock = vi.fn(async (): Promise<SpawnView[]> => []);
vi.mock("./api/spawnlet", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api/spawnlet")>();
  return {
    ...actual,
    listSpawns: () => listSpawnsMock(),
    createSpawn: vi.fn(async () => "new-id"),
    renameSpawn: vi.fn(async () => {}),
    suspendSpawn: vi.fn(async () => {}),
    resumeSpawn: vi.fn(async () => {}),
    deleteSpawn: vi.fn(async () => {}),
  };
});

// Capture each ReconnectingSocket instance so tests can assert openSession fired. The stub records
// the url it was opened with; close()/send() are no-ops.
const sockets: { url: string }[] = [];
vi.mock("./shell/reconnectingSocket", () => ({
  ReconnectingSocket: class {
    url: string;
    constructor(url: string) { this.url = url; sockets.push({ url }); }
    close() {}
    send() {}
  },
}));

import { App } from "./App";

const ACTIVE_SPAWN: SpawnView = { spawnId: "s1", name: "My Spawn", appId: "spawnery/wiki", status: "active", mode: "" };

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
  sockets.length = 0;
  listSpawnsMock.mockReset();
  listSpawnsMock.mockResolvedValue([ACTIVE_SPAWN]);
  document.title = "";
  // jsdom has no matchMedia; theme.initialTheme() calls it on mount.
  (window as any).matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener: () => {}, removeEventListener: () => {} });
});
afterEach(() => { vi.clearAllTimers(); });

describe("App URL-authoritative nav", () => {
  it("navigating to /spawn/<id> binds that spawn (opens its ws)", async () => {
    renderAt("/spawn/s1");
    // The reconciliation effect runs bindSpawn("s1"); since s1 is active+non-tmux it opens the ws.
    await waitFor(() => {
      expect(sockets.some((s) => s.url.includes("/ws/session"))).toBe(true);
    });
  });

  it("does not open a ws when nav is a non-spawn section", async () => {
    renderAt("/templates");
    // Give the poll a tick to run; with no active spawn bound, no session ws should open.
    await waitFor(() => expect(listSpawnsMock).toHaveBeenCalled());
    expect(sockets.some((s) => s.url.includes("/ws/session"))).toBe(false);
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
});
