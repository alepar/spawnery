import { Button } from "@/components/ui/button";

export type View = "chat" | "market" | "settings";

const NAV: { id: View; label: string }[] = [
  { id: "chat", label: "Chat" },
  { id: "market", label: "Marketplace" },
  { id: "settings", label: "Settings" },
];

export function Sidebar({ view, onSelect }: { view: View; onSelect: (v: View) => void }) {
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
      <div className="px-2 text-xs text-muted-foreground/70">— none yet —</div>
    </nav>
  );
}
