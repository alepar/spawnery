export interface Frame {
  seq?: number;
  kind: "user" | "agent" | "thought" | "tool" | "turn" | "perm_request" | "reset" | "prompt" | "perm_response";
  text?: string;
  toolId?: string; title?: string; status?: string;
  state?: "busy" | "idle"; queued?: number;
  reqId?: string; allow?: boolean;
  fromSeq?: number;
}
const enc = new TextEncoder();
export function encodePrompt(text: string): Uint8Array { return enc.encode(JSON.stringify({ kind: "prompt", text }) + "\n"); }
export function encodePermResponse(reqId: string, allow: boolean): Uint8Array {
  return enc.encode(JSON.stringify({ kind: "perm_response", reqId, allow }) + "\n");
}
export function decodeFrame(line: string): Frame { return JSON.parse(line) as Frame; }
