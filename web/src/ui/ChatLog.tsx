import { ToolCallChip } from "./ToolCallChip";
import { Thoughts } from "./Thoughts";

export type Item =
  | { kind: "user"; text: string }
  | { kind: "agent"; text: string }
  | { kind: "tool"; title: string; status?: string }
  | { kind: "thought"; text: string };

export function ChatLog({ items }: { items: Item[] }) {
  return (
    <div className="log">
      {items.map((it, i) => {
        if (it.kind === "tool") return <ToolCallChip key={i} title={it.title} status={it.status} />;
        if (it.kind === "thought") return <Thoughts key={i} text={it.text} />;
        return <div key={i} className={`bubble ${it.kind}`}>{it.text}</div>;
      })}
    </div>
  );
}
