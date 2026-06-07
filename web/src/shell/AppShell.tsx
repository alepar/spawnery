import { Toaster } from "@/components/ui/sonner";
import { Sidebar, type SpawnActions } from "./Sidebar";
import { ConnStatus } from "./ConnStatus";
import type { ConnState } from "./useConnStatus";
import { ChatView } from "@/views/ChatView";
import { TerminalView } from "@/views/TerminalView";
import { TemplatesView } from "@/views/TemplatesView";
import { SettingsView } from "@/views/SettingsView";
import type { Item, TurnState } from "@/views/chat/types";
import type { SpawnView } from "@/api/spawnlet";
import type { Nav } from "@/nav/nav";

// The top-level pane is derived from nav (the URL is authoritative). spawn -> chat; settings ->
// settings; the whole Templates surface (templates/app/my-apps/publish) -> templates.
type TopView = "chat" | "templates" | "settings";
function topView(section: Nav["section"]): TopView {
  if (section === "spawn") return "chat";
  if (section === "settings") return "settings";
  return "templates";
}

export function AppShell({ conn, items, turn, canSend, onSend, perm, onSpawnApp, spawns = [], activeId, actions, onTermConn, nav, navigate }: {
  conn: ConnState | null;
  items: Item[];
  turn: TurnState;
  canSend: boolean;
  onSend: (t: string) => void;
  perm: { title: string; resolve: (b: boolean) => void } | null;
  onSpawnApp: (appId: string, image?: string, runnableId?: string) => void;
  spawns?: SpawnView[];
  activeId?: string | null;
  actions?: SpawnActions;
  // tmux spawns' TerminalView reports its socket state here -> the chat-header ConnStatus dot.
  onTermConn?: (s: "connecting" | "connected" | "reconnecting") => void;
  // URL-authoritative nav, threaded from App: the rendered pane/tab and every click are derived from
  // and routed through it. App's handlers (onSpawnApp, actions.onSelectSpawn/onResume) already navigate.
  nav: Nav;
  navigate: (nav: Nav, opts?: { replace?: boolean }) => void;
}) {
  const view = topView(nav.section);
  const activeSpawn = spawns.find((s) => s.spawnId === activeId);
  const activeName = activeSpawn?.name;
  const activeMode = activeSpawn?.mode;
  const headerLabel = view === "templates" ? "Templates" : view === "settings" ? "Settings" : activeName || "Chat";
  return (
    <div className="flex h-screen bg-background text-foreground">
      <Sidebar nav={nav} navigate={navigate} spawns={spawns} actions={actions} />
      <div className="flex flex-1 flex-col">
        <header className="flex items-center justify-between border-b border-border px-4 py-2">
          <span className="text-sm font-medium">{headerLabel}</span>
          <ConnStatus conn={conn} />
        </header>
        <main className="flex-1 overflow-hidden">
          {/* TerminalView for tmux spawns, else ChatView. A freshly-created tmux spawn has mode=""
              until the first listSpawns refresh, so it briefly shows ChatView — harmless because
              App.tsx only opens the ACP session once status flips to "active" (same refresh that
              carries the mode), so no stray ACP session opens for a tmux spawn. */}
          {view === "chat" && activeMode === "tmux" && activeId && <TerminalView spawnId={activeId} onConn={onTermConn} />}
          {view === "chat" && activeMode !== "tmux" && <ChatView items={items} turn={turn} canSend={canSend} onSend={onSend} perm={perm} focusKey={activeId} />}
          {view === "templates" && <TemplatesView nav={nav} navigate={navigate} onSpawn={onSpawnApp} />}
          {view === "settings" && <SettingsView />}
        </main>
      </div>
      <Toaster theme={typeof document !== "undefined" && document.documentElement.classList.contains("dark") ? "dark" : "light"} />
    </div>
  );
}
