export type Item =
  | { id: number; kind: "user"; text: string; pending?: boolean }
  | { id: number; kind: "agent"; text: string }
  | { id: number; kind: "tool"; title: string; status?: string }
  | { id: number; kind: "thought"; text: string };

export type TurnState = { state: "busy" | "idle"; queued: number };
