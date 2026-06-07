import type { ContentBlock, Diff, ErrorInfo, PermOption } from "@/acp/frames";

export type Item =
  | { id: number; kind: "user"; text: string; pending?: boolean }
  | { id: number; kind: "agent"; text: string }
  | {
      id: number;
      kind: "tool";
      toolId?: string;
      title: string;
      status?: string;
      content?: ContentBlock[];
      diff?: Diff;
      rawInput?: unknown;
      rawOutput?: unknown;
    }
  | { id: number; kind: "thought"; text: string };

// TurnState mirrors the `turn` Frame. reason/error are populated when a turn ends for a non-normal
// reason (cat G); a normal end_turn leaves them undefined so the UI shows nothing new.
export type TurnState = { state: "busy" | "idle"; queued: number; reason?: string; error?: ErrorInfo };

// PermPrompt is an active permission request shown in the modal. options are the agent's real kinded
// choices (cat H); resolve sends the picked optionId ("" = dismissed -> the node auto-denies).
export type PermPrompt = { title: string; options: PermOption[]; resolve: (optionId: string) => void };
