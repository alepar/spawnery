import { MessageList } from "./chat/MessageList";
import { PromptInput } from "./chat/PromptInput";
import { PermissionModal } from "./chat/PermissionModal";
import type { Item, TurnState, PermPrompt } from "./chat/types";
import { turnEndLabel, usageBadge } from "@/lib/turn";

export function ChatView({ items, turn, canSend, onSend, perm, focusKey }: {
  items: Item[];
  turn: TurnState;
  canSend: boolean;
  onSend: (t: string) => void;
  perm: PermPrompt | null;
  focusKey?: string | null;
}) {
  return (
    <div className="flex h-full flex-col">
      <MessageList
        items={items}
        working={turn.state === "busy"}
        queued={turn.queued}
        endLabel={turn.state === "idle" ? turnEndLabel(turn) : null}
        usageLabel={turn.state === "idle" ? usageBadge(turn.usage) : null}
      />
      <PromptInput disabled={!canSend} onSend={onSend} focusKey={focusKey} />
      {perm && <PermissionModal title={perm.title} options={perm.options} onResolve={perm.resolve} />}
    </div>
  );
}
