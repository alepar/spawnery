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
  return (
    <div
      data-role={item.kind}
      className={cn("mx-auto max-w-[70ch] px-4 py-3 text-foreground", isUser && "text-right")}
    >
      {isUser ? (
        <span className="inline-block rounded-lg bg-muted px-3 py-2 text-left">{item.text}</span>
      ) : (
        <Streamdown>{item.text}</Streamdown>
      )}
    </div>
  );
});

export function MessageList({ items }: { items: Item[] }) {
  return (
    <Virtuoso
      className="flex-1"
      data={items}
      followOutput="smooth"
      computeItemKey={(_, item) => item.id}
      itemContent={(_, item) => <Row item={item} />}
    />
  );
}
