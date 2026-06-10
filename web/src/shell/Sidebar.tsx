import { useState } from "react";
import { Button } from "@/components/ui/button";
import { spawnLifecycleAction, type SpawnView, type SpawnStatus } from "@/api/spawnlet";
import type { Nav } from "@/nav/nav";

// SpawnActions is the callback bag App passes down for spawn lifecycle controls.
export interface SpawnActions {
  onSelectSpawn: (spawnId: string) => void;
  onRename: (spawnId: string, name: string) => void;
  onSuspend: (spawnId: string) => void;
  onResume: (spawnId: string) => void;
  onRecreate: (spawnId: string) => void;
  onStop: (spawnId: string) => void;
}

// The two top-nav buttons map to a target Nav. "Templates" stays highlighted across the whole
// Templates surface (browse/app-detail/my-apps/publish); "Settings" only for the settings section.
const NAV: { id: string; label: string; target: Nav; sections: Nav["section"][] }[] = [
  { id: "templates", label: "Templates", target: { section: "templates" }, sections: ["templates", "app", "my-apps", "publish"] },
  { id: "settings", label: "Settings", target: { section: "settings" }, sections: ["settings"] },
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

export function Sidebar({ nav, navigate, spawns = [], actions }: {
  nav: Nav;
  navigate: (nav: Nav) => void;
  spawns?: SpawnView[];
  actions?: SpawnActions;
}) {
  return (
    <nav data-testid="sidebar" className="flex w-52 flex-col gap-1 border-r border-border bg-card p-3">
      <div className="px-2 pb-3 text-sm font-semibold">Spawnery</div>
      {NAV.map((n) => (
        <Button
          key={n.id}
          data-testid={`nav-${n.id}`}
          variant={n.sections.includes(nav.section) ? "secondary" : "ghost"}
          className="justify-start"
          onClick={() => navigate(n.target)}
        >
          {n.label}
        </Button>
      ))}
      <div className="mt-4 px-2 text-xs text-muted-foreground">Spawns</div>
      {spawns.length === 0 ? (
        <div className="px-2 text-xs text-muted-foreground/70">— none yet —</div>
      ) : (
        spawns.map((s) => (
          <SpawnRow key={s.spawnId} spawn={s} active={nav.section === "spawn" && s.spawnId === nav.spawnId} actions={actions} />
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

  // The single lifecycle menu item follows the actual status (suspend/resume/recreate), so the menu
  // never offers an action the CP would reject; transitional/unknown states render a disabled item.
  const lifecycle = spawnLifecycleAction(spawn.status);
  const dispatchLifecycle = () => {
    setMenu(false);
    if (lifecycle.kind === "suspend") actions?.onSuspend(spawn.spawnId);
    else if (lifecycle.kind === "resume") actions?.onResume(spawn.spawnId);
    else if (lifecycle.kind === "recreate") actions?.onRecreate(spawn.spawnId);
  };

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
          {lifecycle.kind === "pending" ? (
            <button data-testid={`spawn-pending-${spawn.spawnId}`} disabled className="rounded px-2 py-1 text-left text-sm text-muted-foreground/50 cursor-default">
              {lifecycle.label}
            </button>
          ) : (
            <button data-testid={`spawn-${lifecycle.kind}-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm hover:bg-accent" onClick={dispatchLifecycle}>
              {lifecycle.label}
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
