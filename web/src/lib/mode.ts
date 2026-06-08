import type { ModePayload } from "@/acp/frames";

// mergeMode folds an incoming `mode` frame into the prior mode state (cat F). The session/new
// advertisement carries both current + the full available set; a current_mode_update carries only the
// new current. So: take the new available list when present, else keep the prior one; always adopt the
// new current. A frame with neither leaves the state unchanged.
export function mergeMode(prev: ModePayload | null, next: ModePayload | undefined): ModePayload | null {
  if (!next) return prev;
  const available = next.available && next.available.length > 0 ? next.available : prev?.available ?? [];
  const current = next.current ?? prev?.current ?? "";
  return { current, available };
}
