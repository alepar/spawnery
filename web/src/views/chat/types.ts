import type { ContentBlock, Diff } from "@/acp/frames";

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

export type TurnState = { state: "busy" | "idle"; queued: number };
