import { MessageList } from "./chat/MessageList";
import { PromptInput } from "./chat/PromptInput";
import { PermissionModal } from "./chat/PermissionModal";
import type { Item, TurnState } from "./chat/types";

export function ChatView({ items, turn, canSend, onSend, perm, focusKey }: {
  items: Item[];
  turn: TurnState;
  canSend: boolean;
  onSend: (t: string) => void;
  perm: { title: string; resolve: (b: boolean) => void } | null;
  focusKey?: string | null;
}) {
  return (
    <div className="flex h-full flex-col">
      <MessageList items={items} working={turn.state === "busy"} queued={turn.queued} />
      <PromptInput disabled={!canSend} onSend={onSend} focusKey={focusKey} />
      {perm && <PermissionModal title={perm.title} onResolve={perm.resolve} />}
    </div>
  );
}
