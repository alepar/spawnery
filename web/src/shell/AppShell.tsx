import { useState } from "react";
import { Toaster } from "@/components/ui/sonner";
import { Sidebar, type View, type SpawnActions } from "./Sidebar";
import { ConnStatus } from "./ConnStatus";
import type { ConnState } from "./useConnStatus";
import { ChatView } from "@/views/ChatView";
import { TerminalView } from "@/views/TerminalView";
import { MarketplaceView } from "@/views/MarketplaceView";
import { SettingsView } from "@/views/SettingsView";
import type { Item, TurnState, PermPrompt } from "@/views/chat/types";
import type { Command, ModePayload } from "@/acp/frames";
import type { SpawnView } from "@/api/spawnlet";

export function AppShell({ conn, items, turn, canSend, onSend, perm, onSpawnApp, spawns = [], activeId, actions, onTermConn, commands, mode, onSetMode, onCancel }: {
  conn: ConnState | null;
  items: Item[];
  turn: TurnState;
  canSend: boolean;
  onSend: (t: string) => void;
  perm: PermPrompt | null;
  onSpawnApp: (appId: string, image?: string, runnableId?: string) => void;
  spawns?: SpawnView[];
  activeId?: string | null;
  actions?: SpawnActions;
  // tmux spawns' TerminalView reports its socket state here -> the chat-header ConnStatus dot.
  onTermConn?: (s: "connecting" | "connected" | "reconnecting") => void;
  commands?: Command[];
  mode?: ModePayload | null; // session modes for the active spawn (cat F)
  onSetMode?: (modeId: string) => void;
  onCancel?: () => void; // interrupt the in-flight turn (cat J)
}) {
  const [view, setView] = useState<View>("market");
  const onSpawn = (appId: string, image?: string, runnableId?: string) => { onSpawnApp(appId, image, runnableId); setView("chat"); };
  // Selecting or resuming a spawn also navigates to its chat.
  const wrapped: SpawnActions | undefined = actions && {
    ...actions,
    onSelectSpawn: (id) => { actions.onSelectSpawn(id); setView("chat"); },
    onResume: (id) => { actions.onResume(id); setView("chat"); },
  };
  const activeSpawn = spawns.find((s) => s.spawnId === activeId);
  const activeName = activeSpawn?.name;
  const activeMode = activeSpawn?.mode;
  return (
    <div className="flex h-screen bg-background text-foreground">
      <Sidebar view={view} onSelect={setView} spawns={spawns} activeId={activeId} actions={wrapped} />
      <div className="flex flex-1 flex-col">
        <header className="flex items-center justify-between border-b border-border px-4 py-2">
          <span className="text-sm font-medium">
            {view === "market" ? "Marketplace" : view === "settings" ? "Settings" : activeName || "Chat"}
          </span>
          <ConnStatus conn={conn} />
        </header>
        <main className="flex-1 overflow-hidden">
          {/* TerminalView for tmux spawns, else ChatView. A freshly-created tmux spawn has mode=""
              until the first listSpawns refresh, so it briefly shows ChatView — harmless because
              App.tsx only opens the ACP session once status flips to "active" (same refresh that
              carries the mode), so no stray ACP session opens for a tmux spawn. */}
          {view === "chat" && activeMode === "tmux" && activeId && <TerminalView spawnId={activeId} onConn={onTermConn} />}
          {view === "chat" && activeMode !== "tmux" && <ChatView items={items} turn={turn} canSend={canSend} onSend={onSend} perm={perm} focusKey={activeId} commands={commands} mode={mode} onSetMode={onSetMode} onCancel={onCancel} />}
          {view === "market" && <MarketplaceView onSpawn={onSpawn} />}
          {view === "settings" && <SettingsView />}
        </main>
      </div>
      <Toaster theme={typeof document !== "undefined" && document.documentElement.classList.contains("dark") ? "dark" : "light"} />
    </div>
  );
}
