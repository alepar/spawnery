import type { ContentBlock, Diff, ErrorInfo } from "@/acp/frames";

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
