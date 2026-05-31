export type Item =
  | { id: number; kind: "user"; text: string }
  | { id: number; kind: "agent"; text: string }
  | { id: number; kind: "tool"; title: string; status?: string }
  | { id: number; kind: "thought"; text: string };
