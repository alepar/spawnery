import { Toaster } from "@/components/ui/sonner";
import { Sidebar, type SpawnActions } from "./Sidebar";
import { SpawnTabs } from "@/sessions/SpawnTabs";
import { TemplatesView } from "@/views/TemplatesView";
import { SettingsView } from "@/views/SettingsView";
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

export function AppShell({ onSpawnApp, spawns = [], activeId, actions, nav, navigate }: {
  onSpawnApp: (appId: string, image?: string, runnableId?: string) => void;
  spawns?: SpawnView[];
  activeId?: string | null;
  actions?: SpawnActions;
  // URL-authoritative nav, threaded from App: the rendered pane/tab and every click are derived from
  // and routed through it. App's handlers (onSpawnApp, actions.onSelectSpawn/onResume) already navigate.
  nav: Nav;
  navigate: (nav: Nav, opts?: { replace?: boolean }) => void;
}) {
  const view = topView(nav.section);
  const activeSpawn = spawns.find((s) => s.spawnId === activeId);
  const headerLabel = view === "templates" ? "Templates" : view === "settings" ? "Settings" : activeSpawn?.name || "Chat";
  return (
    <div className="flex h-screen bg-background text-foreground">
      <Sidebar nav={nav} navigate={navigate} spawns={spawns} actions={actions} />
      <div className="flex flex-1 flex-col">
        <header className="flex items-center justify-between border-b border-border px-4 py-2">
          <span className="text-sm font-medium">{headerLabel}</span>
        </header>
        <main className="flex-1 overflow-hidden">
          {/* SpawnTabs stays mounted whenever a spawn is active (keyed on spawnId, so a spawn switch
              remounts it) and is hidden when off the spawn view, preserving background keep-alive. */}
          {activeId && (
            <div className="h-full" style={{ display: view === "chat" ? "block" : "none" }}>
              <SpawnTabs key={activeId} spawnId={activeId} spawn={activeSpawn} />
            </div>
          )}
          {view === "templates" && <TemplatesView nav={nav} navigate={navigate} onSpawn={onSpawnApp} />}
          {view === "settings" && <SettingsView />}
        </main>
      </div>
      <Toaster theme={typeof document !== "undefined" && document.documentElement.classList.contains("dark") ? "dark" : "light"} />
    </div>
  );
}
