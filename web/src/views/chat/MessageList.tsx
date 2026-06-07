import { memo } from "react";
import { Virtuoso } from "react-virtuoso";
import { Streamdown } from "streamdown";
import { cn } from "@/lib/utils";
import type { Item } from "./types";
import { ToolCallChip } from "./ToolCallChip";
import { Thoughts } from "./Thoughts";
import { PlanChecklist } from "./PlanChecklist";

const Row = memo(function Row({ item }: { item: Item }) {
  if (item.kind === "tool")
    return (
      <ToolCallChip
        title={item.title}
        status={item.status}
        content={item.content}
        diff={item.diff}
        rawInput={item.rawInput}
        rawOutput={item.rawOutput}
      />
    );
  if (item.kind === "thought") return <Thoughts text={item.text} />;
  if (item.kind === "plan") return <PlanChecklist entries={item.entries} />;

  const isUser = item.kind === "user";
  const pending = isUser && item.pending;
  return (
    <div data-role={item.kind} className={cn("mx-auto max-w-[70ch] px-4 py-3 text-foreground", isUser && "text-right")}>
      {isUser ? (
        <span className={cn("inline-block rounded-lg bg-muted px-3 py-2 text-left", pending && "opacity-50")}>
          {item.text}
          {pending && (
            <span data-testid="queued-tag" className="ml-2 align-middle text-[10px] uppercase tracking-wide text-muted-foreground">
              queued
            </span>
          )}
        </span>
      ) : (<Streamdown>{item.text}</Streamdown>)}
    </div>
  );
});

type ListContext = { working: boolean; queued: number; endLabel?: string | null; usageLabel?: string | null };

// WorkingFooter renders the transcript-footer typing indicator (pulsing dots + "working…[· N queued]")
// while the agent is mid-turn. When the turn has instead ended for a non-normal reason (cat G), it
// renders an honest turn-ending indicator (e.g. "stopped: max tokens" / "cancelled" / an error)
// instead of silently going idle, plus a per-turn token/cost usage badge when the agent reported usage
// (cat D, guarded — absent agents show no badge). A normal usage-less end_turn shows nothing. It is the
// list Footer so it sits at the end of the conversation and scrolls with followOutput.
function WorkingFooter({ context }: { context?: ListContext }) {
  if (!context?.working) {
    const { endLabel, usageLabel } = context ?? {};
    if (endLabel || usageLabel) {
      return (
        <div className="mx-auto flex max-w-[70ch] items-center gap-2 px-4 py-3 text-xs text-muted-foreground">
          {endLabel && <span data-testid="turn-ended-indicator">{endLabel}</span>}
          {usageLabel && <span data-testid="turn-usage-badge">{usageLabel}</span>}
        </div>
      );
    }
    return null;
  }
  return (
    <div data-testid="working-indicator" className="mx-auto flex max-w-[70ch] items-center gap-2 px-4 py-3 text-xs text-muted-foreground">
      <span className="flex gap-1">
        <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-current" />
        <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-current [animation-delay:150ms]" />
        <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-current [animation-delay:300ms]" />
      </span>
      working…{context.queued > 0 ? ` · ${context.queued} queued` : ""}
    </div>
  );
}

export function MessageList({ items, working = false, queued = 0, endLabel = null, usageLabel = null }: { items: Item[]; working?: boolean; queued?: number; endLabel?: string | null; usageLabel?: string | null }) {
  return (
    <Virtuoso
      className="flex-1"
      data={items}
      followOutput="smooth"
      // Keep the latest visible as messages stream in. A streaming agent reply grows the LAST item
      // rather than appending a new one, and the growth nudges the scroll past the default 4px
      // "at bottom" gate — which stops followOutput. A generous threshold keeps it pinned while
      // streaming; if the user scrolls up to read history, they leave the threshold and it stops
      // (so we don't yank them back). Start at the bottom on (re)mount so replayed history opens latest.
      atBottomThreshold={160}
      initialTopMostItemIndex={Math.max(0, items.length - 1)}
      context={{ working, queued, endLabel, usageLabel }}
      components={{ Footer: WorkingFooter }}
      computeItemKey={(_, item) => item.id}
      itemContent={(_, item) => <Row item={item} />}
    />
  );
}
