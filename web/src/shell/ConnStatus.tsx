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

// ConnStatus renders a per-session WS connection light as a bare dot; the state label (connecting/
// connected/…) is exposed via the native `title` hover tooltip (and `aria-label`) rather than inline
// text, so a tab heading shows only the dot + tab name. Renders nothing when there is no live socket.
export function ConnStatus({ conn }: { conn: ConnState | null }) {
  if (!conn) return null;
  return (
    <span
      data-testid="status"
      data-status={conn}
      title={LABEL[conn]}
      aria-label={LABEL[conn]}
      className="inline-flex items-center"
    >
      <span className={`inline-block h-2 w-2 rounded-full ${DOT[conn]}`} />
    </span>
  );
}
