import type { ContentBlock, Diff, ErrorInfo, PermOption, PlanEntry } from "@/acp/frames";

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
  | { id: number; kind: "thought"; text: string }
  // The agent's plan/todo checklist (cat C). Replace-in-place: one plan item, swapped wholesale by
  // each plan frame (see lib/plan.ts). entries advance pending -> in_progress -> completed.
  | { id: number; kind: "plan"; entries: PlanEntry[] };

// TurnState mirrors the `turn` Frame. reason/error are populated when a turn ends for a non-normal
// reason (cat G); a normal end_turn leaves them undefined so the UI shows nothing new.
export type TurnState = { state: "busy" | "idle"; queued: number; reason?: string; error?: ErrorInfo };

// PermPrompt is an active permission request shown in the modal. options are the agent's real kinded
// choices (cat H); resolve sends the picked optionId ("" = dismissed -> the node auto-denies).
export type PermPrompt = { title: string; options: PermOption[]; resolve: (optionId: string) => void };
