import { memo } from "react";
import { Virtuoso } from "react-virtuoso";
import { Streamdown } from "streamdown";
import { cn } from "@/lib/utils";
import type { Item } from "./types";
import { ToolCallChip } from "./ToolCallChip";
import { Thoughts } from "./Thoughts";

const Row = memo(function Row({ item }: { item: Item }) {
  if (item.kind === "tool") return <ToolCallChip title={item.title} status={item.status} />;
  if (item.kind === "thought") return <Thoughts text={item.text} />;

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

type ListContext = { working: boolean; queued: number };

// WorkingFooter renders the transcript-footer typing indicator (pulsing dots + "working…[· N queued]")
// while the agent is mid-turn. It is the list Footer so it sits at the end of the conversation and
// scrolls with followOutput.
function WorkingFooter({ context }: { context?: ListContext }) {
  if (!context?.working) return null;
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

export function MessageList({ items, working = false, queued = 0 }: { items: Item[]; working?: boolean; queued?: number }) {
  return (
    <Virtuoso
      className="flex-1"
      data={items}
      followOutput="smooth"
      context={{ working, queued }}
      components={{ Footer: WorkingFooter }}
      computeItemKey={(_, item) => item.id}
      itemContent={(_, item) => <Row item={item} />}
    />
  );
}
