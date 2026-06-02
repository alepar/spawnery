import { Button } from "@/components/ui/button";

export type View = "chat" | "market" | "settings";

const NAV: { id: View; label: string }[] = [
  { id: "market", label: "Marketplace" },
  { id: "settings", label: "Settings" },
];

export function Sidebar({ view, onSelect, activeSpawn }: { view: View; onSelect: (v: View) => void; activeSpawn?: { label: string } | null }) {
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
      {activeSpawn ? (
        <Button
          data-testid="nav-spawn"
          variant={view === "chat" ? "secondary" : "ghost"}
          className="justify-start"
          onClick={() => onSelect("chat")}
        >
          {activeSpawn.label}
        </Button>
      ) : (
        <div className="px-2 text-xs text-muted-foreground/70">— none yet —</div>
      )}
    </nav>
  );
}
