import type { ModePayload } from "@/acp/frames";

// ModeSelector renders the agent's selectable session modes (cat F) as a dropdown, with the agent's
// reported current mode selected. Changing it sends an upward set_mode; the selector then FOLLOWS the
// agent's current_mode_update (the value is driven by `mode.current`, not local state). Graceful
// absence: an agent with no modes (or fewer than two) renders nothing — no selector at all.
export function ModeSelector({ mode, onSetMode }: { mode: ModePayload | null | undefined; onSetMode: (modeId: string) => void }) {
  const available = mode?.available ?? [];
  if (available.length < 2) return null; // nothing to switch between -> no selector
  const current = mode?.current ?? available[0]?.id ?? "";
  return (
    <div data-testid="mode-selector" className="flex items-center gap-2 px-4 pt-2 text-xs text-muted-foreground">
      <label htmlFor="mode-select" className="select-none">Mode</label>
      <select
        id="mode-select"
        aria-label="Session mode"
        value={current}
        onChange={(e) => onSetMode(e.target.value)}
        className="rounded border border-border bg-background px-2 py-1 text-xs text-foreground"
      >
        {available.map((m) => (
          <option key={m.id} value={m.id}>{m.name || m.id}</option>
        ))}
      </select>
    </div>
  );
}
