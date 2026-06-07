import type { ContentBlock } from "@/acp/frames";

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
      rawInput?: unknown;
      rawOutput?: unknown;
    }
  | { id: number; kind: "thought"; text: string };

export type TurnState = { state: "busy" | "idle"; queued: number };
