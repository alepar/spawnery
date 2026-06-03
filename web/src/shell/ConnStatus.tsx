import type { ConnState } from "./useConnStatus";

// Both transitional states pulse; terminal states are static.
const DOT: Record<ConnState, string> = {
  waiting: "bg-zinc-400 animate-pulse",
  connecting: "bg-zinc-400 animate-pulse",
  reconnecting: "bg-yellow-400 animate-pulse",
  slow: "bg-amber-500 animate-pulse",
  connected: "bg-green-500",
  error: "bg-red-500",
  disconnected: "bg-red-500",
};
const LABEL: Record<ConnState, string> = {
  waiting: "waiting",
  connecting: "connecting…",
  reconnecting: "reconnecting…",
  slow: "connecting…",
  connected: "connected",
  error: "error",
  disconnected: "disconnected",
};

// ConnStatus renders the chat-header WS connection light + label. It renders nothing when there is
// no live socket (conn === null).
export function ConnStatus({ conn }: { conn: ConnState | null }) {
  if (!conn) return null;
  return (
    <span data-testid="status" data-status={conn} className="flex items-center gap-1.5 text-xs text-muted-foreground">
      <span className={`inline-block h-2 w-2 rounded-full ${DOT[conn]}`} />
      {LABEL[conn]}
    </span>
  );
}
