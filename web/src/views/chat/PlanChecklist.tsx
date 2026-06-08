import { cn } from "@/lib/utils";
import type { PlanEntry } from "@/acp/frames";

// statusIcon maps a plan-entry status to a checklist glyph: ☐ pending, ◐ in_progress, ☑ completed.
function statusIcon(status?: string): string {
  switch (status) {
    case "in_progress":
      return "◐";
    case "completed":
      return "☑";
    default:
      return "☐";
  }
}

// PlanChecklist renders the agent's plan/todo list as a checklist. Replace-in-place lives upstream
// (lib/plan.ts swaps the whole entries array); this component just renders the current entries by
// status. Graceful absence: an empty plan renders nothing (no empty panel) — goose, which never emits
// a plan, shows no checklist at all.
export function PlanChecklist({ entries }: { entries: PlanEntry[] }) {
  if (!entries || entries.length === 0) return null;
  return (
    <div data-testid="plan-checklist" data-role="plan" className="mx-auto max-w-[70ch] px-4 py-2">
      <div className="rounded-md border border-border bg-muted/40 p-3">
        <div className="mb-1 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">Plan</div>
        <ul className="space-y-1">
          {entries.map((e, i) => {
            const done = e.status === "completed";
            return (
              <li
                key={i}
                data-testid="plan-entry"
                data-status={e.status ?? "pending"}
                className={cn(
                  "flex items-start gap-2 text-sm",
                  done && "text-muted-foreground line-through",
                  e.status === "in_progress" && "text-foreground font-medium",
                )}
              >
                <span aria-hidden className="select-none leading-5">{statusIcon(e.status)}</span>
                <span className="leading-5">{e.content}</span>
              </li>
            );
          })}
        </ul>
      </div>
    </div>
  );
}
