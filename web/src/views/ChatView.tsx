import { MessageList } from "./chat/MessageList";
import { PromptInput } from "./chat/PromptInput";
import { PermissionModal } from "./chat/PermissionModal";
import type { Item } from "./chat/types";

export function ChatView({ items, busy, onSend, perm }: {
  items: Item[];
  busy: boolean;
  onSend: (t: string) => void;
  perm: { title: string; resolve: (b: boolean) => void } | null;
}) {
  return (
    <div className="flex h-full flex-col">
      <MessageList items={items} />
      <PromptInput disabled={busy} onSend={onSend} />
      {perm && <PermissionModal title={perm.title} onResolve={perm.resolve} />}
    </div>
  );
}
