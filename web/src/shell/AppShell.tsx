import { Toaster } from "@/components/ui/sonner";
import { Sidebar, type SpawnActions } from "./Sidebar";
import { SpawnTabs } from "@/sessions/SpawnTabs";
import { ProvisioningPane } from "@/sessions/ProvisioningPane";
import { TemplatesView } from "@/views/TemplatesView";
import { SettingsView } from "@/views/SettingsView";
import { ProfilesView } from "@/views/ProfilesView";
import type { SpawnView, CreateMountBinding } from "@/api/spawnlet";
import type { Nav } from "@/nav/nav";

// The top-level pane is derived from nav (the URL is authoritative). spawn -> chat; settings ->
// settings; profiles -> profiles; the whole Templates surface (templates/app/my-apps/publish) -> templates.
type TopView = "chat" | "templates" | "settings" | "profiles";
function topView(section: Nav["section"]): TopView {
  if (section === "spawn") return "chat";
  if (section === "settings") return "settings";
  if (section === "profiles") return "profiles";
  return "templates";
}

export function AppShell({ onSpawnApp, spawns = [], activeId, actions, nav, navigate }: {
  onSpawnApp: (appId: string, image?: string, runnableId?: string, profileId?: string, mounts?: CreateMountBinding[]) => void;
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
  const headerLabel =
    view === "templates" ? "Templates" :
    view === "settings" ? "Settings" :
    view === "profiles" ? "Profiles" :
    activeSpawn?.name || "Chat";
  return (
    <div className="flex h-screen bg-background text-foreground">
      <Sidebar nav={nav} navigate={navigate} spawns={spawns} actions={actions} />
      <div className="flex flex-1 flex-col">
        <header className="flex items-center justify-between border-b border-border px-4 py-2">
          <span className="text-sm font-medium">{headerLabel}</span>
        </header>
        <main className="flex-1 overflow-hidden">
          {/* SpawnTabs stays mounted whenever a spawn is active (keyed on spawnId, so a spawn switch
              remounts it) and is hidden when off the spawn view, preserving background keep-alive.
              While a spawn is starting or errored, ProvisioningPane replaces SpawnTabs (no sessions yet). */}
          {activeId && (
            <div className="h-full overflow-auto" style={{ display: view === "chat" ? "block" : "none" }}>
              {activeSpawn && (activeSpawn.status === "starting" || activeSpawn.status === "error")
                ? <ProvisioningPane spawn={activeSpawn} />
                : <SpawnTabs key={activeId} spawnId={activeId} />}
            </div>
          )}
          {view === "templates" && <TemplatesView nav={nav} navigate={navigate} onSpawn={onSpawnApp} />}
          {view === "settings" && <SettingsView />}
          {view === "profiles" && <ProfilesView />}
        </main>
      </div>
      <Toaster theme={typeof document !== "undefined" && document.documentElement.classList.contains("dark") ? "dark" : "light"} />
    </div>
  );
}
