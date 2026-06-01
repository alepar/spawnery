import { useState } from "react";
import { Toaster } from "@/components/ui/sonner";
import { Sidebar, type View } from "./Sidebar";
import { ChatView } from "@/views/ChatView";
import { MarketplaceView } from "@/views/MarketplaceView";
import { SettingsView } from "@/views/SettingsView";
import type { Item } from "@/views/chat/types";

export function AppShell({ status, items, busy, onSend, perm, onSpawnApp }: {
  status: string;
  items: Item[];
  busy: boolean;
  onSend: (t: string) => void;
  perm: { title: string; resolve: (b: boolean) => void } | null;
  onSpawnApp: (appId: string) => void;
}) {
  const [view, setView] = useState<View>("chat");
  const onSpawn = (appId: string) => { onSpawnApp(appId); setView("chat"); };
  return (
    <div className="flex h-screen bg-background text-foreground">
      <Sidebar view={view} onSelect={setView} />
      <div className="flex flex-1 flex-col">
        <header className="flex items-center justify-between border-b border-border px-4 py-2">
          <span className="text-sm font-medium">{view === "market" ? "Marketplace" : view === "settings" ? "Settings" : "Chat"}</span>
          <span data-testid="status" className="text-xs text-muted-foreground">{status}</span>
        </header>
        <main className="flex-1 overflow-hidden">
          {view === "chat" && <ChatView items={items} busy={busy} onSend={onSend} perm={perm} />}
          {view === "market" && <MarketplaceView onSpawn={onSpawn} />}
          {view === "settings" && <SettingsView />}
        </main>
      </div>
      <Toaster theme={typeof document !== "undefined" && document.documentElement.classList.contains("dark") ? "dark" : "light"} />
    </div>
  );
}
