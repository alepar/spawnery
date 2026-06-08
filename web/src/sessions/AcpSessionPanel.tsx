import { useEffect, useRef } from "react";
import { Conn } from "@/acp/conn";
import { encodePrompt, encodePermResponse, type Frame } from "@/acp/frames";
import { ReconnectingSocket } from "@/shell/reconnectingSocket";
import { DEV_TOKEN } from "@/api/spawnlet";
import { ChatView } from "@/views/ChatView";
import { MAX_QUEUED } from "@/lib/turn";
import { useSessionStore } from "./store";

// crypto.randomUUID needs a secure context; fall back so plain-HTTP LAN access still mounts.
function makeClientId(): string {
  try { if (typeof crypto !== "undefined" && crypto.randomUUID) return crypto.randomUUID(); } catch { /* non-secure */ }
  return `a-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}
const CLIENT_ID = makeClientId();

export function AcpSessionPanel({ spawnId, sessionId, active }: {
  spawnId: string;
  sessionId: string;
  active: boolean;
}) {
  const rt = useSessionStore((s) => s.acp[sessionId]);
  const conn = useSessionStore((s) => s.conn[sessionId] ?? null);
  const sockRef = useRef<ReconnectingSocket | null>(null);
  const genRef = useRef(0);

  useEffect(() => {
    const gen = ++genRef.current;
    useSessionStore.getState().setConn(sessionId, "connecting");
    const sock = new ReconnectingSocket(`ws://${location.host}/ws/session`, {
      onOpen: () => {
        if (genRef.current !== gen) return;
        // Fresh frame receiver per (re)connect; wire it BEFORE the bind so replay can't precede onmessage.
        new Conn(sock, (m) => { if (genRef.current === gen) useSessionStore.getState().applyFrame(sessionId, m as Frame); });
        const cursor = useSessionStore.getState().acp[sessionId]?.lastSeq ?? 0;
        sock.send(JSON.stringify({ spawnId, sessionId, clientId: CLIENT_ID, token: DEV_TOKEN, cursor }));
        useSessionStore.getState().setConn(sessionId, "connected");
      },
      onDown: () => { if (genRef.current === gen) useSessionStore.getState().setConn(sessionId, "reconnecting"); },
    });
    sockRef.current = sock;
    return () => {
      // Intentionally bump the LIVE gen so any in-flight reconnect callback self-invalidates — we
      // want the current ref value at teardown time, not a captured snapshot.
      // eslint-disable-next-line react-hooks/exhaustive-deps
      genRef.current++;
      sock.close();
      sockRef.current = null;
      useSessionStore.getState().setConn(sessionId, null);
    };
  }, [spawnId, sessionId]);

  const turn = rt?.turn ?? { state: "idle" as const, queued: 0 };
  const canSend = conn === "connected" && turn.queued < MAX_QUEUED;
  const onSend = (text: string) => sockRef.current?.send(encodePrompt(text));
  const perm = rt?.perm
    ? { title: rt.perm.title, resolve: (allow: boolean) => {
        sockRef.current?.send(encodePermResponse(rt.perm!.reqId, allow));
        useSessionStore.getState().clearPerm(sessionId);
      } }
    : null;

  return (
    <ChatView
      items={rt?.items ?? []}
      turn={turn}
      canSend={canSend}
      onSend={onSend}
      perm={perm}
      focusKey={active ? sessionId : null}
    />
  );
}
