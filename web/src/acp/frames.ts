// Nested payload interfaces — mirror internal/node/frame.go. All optional; populated by sp-ufz.5..13.
export interface ContentBlock { type?: string; text?: string }
export interface Diff { path?: string; oldText?: string; newText?: string }
export interface ToolPayload {
  content?: ContentBlock[];
  diff?: Diff;
  rawInput?: unknown;
  rawOutput?: unknown;
}
export interface PlanEntry { content?: string; priority?: string; status?: string }
export interface Usage { input?: number; output?: number; cached?: number; thought?: number; total?: number; cost?: number }
export interface ErrorInfo { code?: number; message?: string }
export interface Command { name?: string; description?: string; inputHint?: string }
export interface PermOption { optionId?: string; name?: string; kind?: string }
export interface Mode { id?: string; name?: string }
export interface ModePayload { current?: string; available?: Mode[] }

interface FrameBase { seq?: number }

// Frame is a discriminated union keyed on `kind`. Server->client kinds carry payloads; the last few
// are client->node control frames (prompt/perm_response/cancel/set_mode).
export type Frame =
  | (FrameBase & { kind: "user" | "agent" | "thought"; text?: string })
  | (FrameBase & { kind: "tool"; toolId?: string; title?: string; status?: string; tool?: ToolPayload })
  | (FrameBase & { kind: "turn"; state?: "busy" | "idle"; queued?: number; usage?: Usage; reason?: string; error?: ErrorInfo })
  | (FrameBase & { kind: "plan"; plan?: PlanEntry[] })
  | (FrameBase & { kind: "commands"; cmds?: Command[] })
  | (FrameBase & { kind: "mode"; mode?: ModePayload })
  | (FrameBase & { kind: "perm_request"; reqId?: string; title?: string; options?: PermOption[] })
  | (FrameBase & { kind: "reset"; fromSeq?: number })
  | (FrameBase & { kind: "prompt"; text?: string })
  | (FrameBase & { kind: "perm_response"; reqId?: string; optionId?: string })
  | (FrameBase & { kind: "cancel" })
  | (FrameBase & { kind: "set_mode"; modeId?: string });

const enc = new TextEncoder();
export function encodePrompt(text: string): Uint8Array { return enc.encode(JSON.stringify({ kind: "prompt", text }) + "\n"); }
// optionId is the agent option the user picked (e.g. "allow_once"); "" means dismissed -> the node
// auto-denies (selects a reject-ish option). Replaces the old binary allow boolean (sp-ufz.8).
export function encodePermResponse(reqId: string, optionId: string): Uint8Array {
  return enc.encode(JSON.stringify({ kind: "perm_response", reqId, optionId }) + "\n");
}
// cancel / set_mode encoders mirror the existing upward frames. pump-side wiring lands in sp-ufz.12/.13.
export function encodeCancel(): Uint8Array { return enc.encode(JSON.stringify({ kind: "cancel" }) + "\n"); }
export function encodeSetMode(modeId: string): Uint8Array { return enc.encode(JSON.stringify({ kind: "set_mode", modeId }) + "\n"); }
export function decodeFrame(line: string): Frame { return JSON.parse(line) as Frame; }
