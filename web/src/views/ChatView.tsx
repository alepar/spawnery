import { MessageList } from "./chat/MessageList";
import { PromptInput } from "./chat/PromptInput";
import { PermissionModal } from "./chat/PermissionModal";
import { ModeSelector } from "./chat/ModeSelector";
import { ModelControl } from "./chat/ModelControl";
import { StopButton } from "./chat/StopButton";
import type { Item, TurnState, PermPrompt } from "./chat/types";
import type { Command, ModePayload } from "@/acp/frames";
import { turnEndLabel, usageBadge } from "@/lib/turn";

export function ChatView({ items, turn, canSend, onSend, perm, focusKey, commands, mode, onSetMode, onCancel, spawnId, model, modelApplied }: {
  items: Item[];
  turn: TurnState;
  canSend: boolean;
  onSend: (t: string) => void;
  perm: PermPrompt | null;
  focusKey?: string | null;
  commands?: Command[];
  mode?: ModePayload | null;
  onSetMode?: (modeId: string) => void;
  onCancel?: () => void;
  spawnId?: string;
  model?: string;
  modelApplied?: boolean;
}) {
  return (
    <div className="flex h-full flex-col">
      {spawnId && <ModelControl spawnId={spawnId} model={model ?? ""} modelApplied={modelApplied ?? true} />}
      <MessageList
        items={items}
        working={turn.state === "busy"}
        queued={turn.queued}
        endLabel={turn.state === "idle" ? turnEndLabel(turn) : null}
        usageLabel={turn.state === "idle" ? usageBadge(turn.usage) : null}
      />
      <ModeSelector mode={mode} onSetMode={onSetMode ?? (() => {})} />
      <StopButton busy={turn.state === "busy"} onCancel={onCancel ?? (() => {})} />
      <PromptInput disabled={!canSend} onSend={onSend} focusKey={focusKey} commands={commands} />
      {perm && <PermissionModal title={perm.title} options={perm.options} onResolve={perm.resolve} />}
    </div>
  );
}
