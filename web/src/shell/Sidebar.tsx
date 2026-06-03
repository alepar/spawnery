import { useState } from "react";
import { Button } from "@/components/ui/button";
import type { SpawnView, SpawnStatus } from "@/api/spawnlet";

export type View = "chat" | "market" | "settings";

// SpawnActions is the callback bag App passes down for spawn lifecycle controls.
export interface SpawnActions {
  onSelectSpawn: (spawnId: string) => void;
  onRename: (spawnId: string, name: string) => void;
  onSuspend: (spawnId: string) => void;
  onResume: (spawnId: string) => void;
  onStop: (spawnId: string) => void;
}

const NAV: { id: View; label: string }[] = [
  { id: "market", label: "Marketplace" },
  { id: "settings", label: "Settings" },
];

// dot color per ledger status: green=online, grey=suspended, red=failed, amber(pulse)=transitional.
const DOT: Record<SpawnStatus, string> = {
  active: "bg-green-500",
  suspended: "bg-zinc-400",
  suspending: "bg-amber-500 animate-pulse",
  starting: "bg-yellow-400 animate-pulse",
  unreachable: "bg-red-500",
  error: "bg-red-500",
  unknown: "bg-zinc-400",
};

export function Sidebar({ view, onSelect, spawns = [], activeId, actions }: {
  view: View;
  onSelect: (v: View) => void;
  spawns?: SpawnView[];
  activeId?: string | null;
  actions?: SpawnActions;
}) {
  return (
    <nav data-testid="sidebar" className="flex w-52 flex-col gap-1 border-r border-border bg-card p-3">
      <div className="px-2 pb-3 text-sm font-semibold">Spawnery</div>
      {NAV.map((n) => (
        <Button
          key={n.id}
          data-testid={`nav-${n.id}`}
          variant={view === n.id ? "secondary" : "ghost"}
          className="justify-start"
          onClick={() => onSelect(n.id)}
        >
          {n.label}
        </Button>
      ))}
      <div className="mt-4 px-2 text-xs text-muted-foreground">Spawns</div>
      {spawns.length === 0 ? (
        <div className="px-2 text-xs text-muted-foreground/70">— none yet —</div>
      ) : (
        spawns.map((s) => (
          <SpawnRow key={s.spawnId} spawn={s} active={view === "chat" && s.spawnId === activeId} actions={actions} />
        ))
      )}
    </nav>
  );
}

function SpawnRow({ spawn, active, actions }: { spawn: SpawnView; active: boolean; actions?: SpawnActions }) {
  const [menu, setMenu] = useState(false);
  const [confirmStop, setConfirmStop] = useState(false);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(spawn.name);

  const startEdit = () => { setMenu(false); setDraft(spawn.name); setEditing(true); };
  const commit = () => {
    setEditing(false);
    const n = draft.trim();
    if (n && n !== spawn.name) actions?.onRename(spawn.spawnId, n);
  };

  return (
    <div
      data-testid={`spawn-row-${spawn.spawnId}`}
      className={`relative flex items-start rounded-md ${active ? "bg-secondary" : "hover:bg-accent"}`}
    >
      <button
        data-testid={`spawn-select-${spawn.spawnId}`}
        className="flex min-w-0 flex-1 flex-col items-start gap-0.5 px-2 py-1.5 text-left"
        onClick={() => actions?.onSelectSpawn(spawn.spawnId)}
      >
        <span className="flex w-full items-center gap-2">
          <span
            data-testid={`spawn-dot-${spawn.spawnId}`}
            data-status={spawn.status}
            className={`inline-block h-2 w-2 shrink-0 rounded-full ${DOT[spawn.status]}`}
          />
          {editing ? (
            <input
              data-testid={`spawn-name-input-${spawn.spawnId}`}
              autoFocus
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onClick={(e) => e.stopPropagation()}
              onKeyDown={(e) => {
                if (e.key === "Enter") commit();
                if (e.key === "Escape") setEditing(false);
              }}
              onBlur={commit}
              className="w-full rounded border border-input bg-background px-1 text-sm"
            />
          ) : (
            <span
              data-testid={`spawn-name-${spawn.spawnId}`}
              className="truncate text-sm font-medium"
              onDoubleClick={(e) => { e.stopPropagation(); startEdit(); }}
            >
              {spawn.name || spawn.appId}
            </span>
          )}
        </span>
        <span className="truncate pl-4 text-xs text-muted-foreground/70">{spawn.appId}</span>
      </button>

      <button
        data-testid={`spawn-kebab-${spawn.spawnId}`}
        className="px-2 py-1.5 text-muted-foreground hover:text-foreground"
        aria-label="spawn actions"
        onClick={() => { setMenu((m) => !m); setConfirmStop(false); }}
      >
        ⋯
      </button>

      {menu && (
        <div
          data-testid={`spawn-menu-${spawn.spawnId}`}
          className="absolute right-1 top-8 z-10 flex flex-col rounded-md border border-border bg-popover p-1 shadow-md"
        >
          <button data-testid={`spawn-rename-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm hover:bg-accent" onClick={startEdit}>
            Rename
          </button>
          {spawn.status === "suspended" ? (
            <button data-testid={`spawn-resume-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm hover:bg-accent" onClick={() => { setMenu(false); actions?.onResume(spawn.spawnId); }}>
              Resume
            </button>
          ) : (
            <button data-testid={`spawn-suspend-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm hover:bg-accent" onClick={() => { setMenu(false); actions?.onSuspend(spawn.spawnId); }}>
              Suspend
            </button>
          )}
          {confirmStop ? (
            <button data-testid={`spawn-stop-confirm-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm text-red-500 hover:bg-accent" onClick={() => { setMenu(false); setConfirmStop(false); actions?.onStop(spawn.spawnId); }}>
              Confirm stop
            </button>
          ) : (
            <button data-testid={`spawn-stop-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm text-red-500 hover:bg-accent" onClick={() => setConfirmStop(true)}>
              Stop
            </button>
          )}
        </div>
      )}
    </div>
  );
}
