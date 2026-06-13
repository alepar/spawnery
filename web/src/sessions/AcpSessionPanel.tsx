import { useEffect, useRef } from "react";
import { Conn } from "@/acp/conn";
import { encodePrompt, encodePermResponse, encodeSetMode, encodeCancel, type Frame } from "@/acp/frames";
import { ReconnectingSocket } from "@/shell/reconnectingSocket";
import { getAccessToken, authEnabled, useSessionStore as useAuthStore } from "@/auth/session";
import { cpWsUrl } from "@/config/endpoints";
import { ChatView } from "@/views/ChatView";
import { MAX_QUEUED } from "@/lib/turn";
import { useSessionStore } from "./store";

// crypto.randomUUID needs a secure context; fall back so plain-HTTP LAN access still mounts.
function makeClientId(): string {
  try { if (typeof crypto !== "undefined" && crypto.randomUUID) return crypto.randomUUID(); } catch { /* non-secure */ }
  return `a-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}
const CLIENT_ID = makeClientId();

export function AcpSessionPanel({ spawnId, sessionId, active, ready }: {
  spawnId: string;
  sessionId: string;
  active: boolean;
  // ready: the roster status is "active" — the node's Pump is registered+ready. Additional acp
  // sessions launch their Pump async (status "starting" first); binding before then attaches to a
  // not-yet-ready session ("send into the void", false "connected" dot). Gate the socket on this.
  ready: boolean;
}) {
  const rt = useSessionStore((s) => s.acp[sessionId]);
  const conn = useSessionStore((s) => s.conn[sessionId] ?? null);
  const sockRef = useRef<ReconnectingSocket | null>(null);
  const genRef = useRef(0);

  useEffect(() => {
    // Not ready yet: don't open the socket. Show an honest grey-pulse "waiting" dot and bail; the
    // effect re-runs (keyed on `ready`) and opens the socket the instant the session flips ready.
    if (!ready) {
      useSessionStore.getState().setConn(sessionId, "waiting");
      return;
    }
    const gen = ++genRef.current;
    useSessionStore.getState().setConn(sessionId, "connecting");
    const sock = new ReconnectingSocket(cpWsUrl("/ws/session"), {
      onOpen: () => {
        if (genRef.current !== gen) return;
        // Fresh frame receiver per (re)connect; wire it BEFORE the bind so replay can't precede onmessage.
        new Conn(sock, (m) => { if (genRef.current === gen) useSessionStore.getState().applyFrame(sessionId, m as Frame); });
        const cursor = useSessionStore.getState().acp[sessionId]?.lastSeq ?? 0;
        sock.send(JSON.stringify({ spawnId, sessionId, clientId: CLIENT_ID, token: getAccessToken(), cursor }));
        useSessionStore.getState().setConn(sessionId, "connected");
      },
      onDown: () => { if (genRef.current === gen) useSessionStore.getState().setConn(sessionId, "reconnecting"); },
    });
    sockRef.current = sock;

    // In-band reauth: ~14min interval (under the 15min ws.go deadline).
    const REAUTH_MS = 14 * 60 * 1000;
    const sendReauth = (token: string) => {
      try { sockRef.current?.send(JSON.stringify({ type: "reauth", token })); } catch { /* socket closing */ }
    };
    const reauthInterval = authEnabled()
      ? setInterval(() => sendReauth(getAccessToken()), REAUTH_MS)
      : null;

    // Push a reauth frame immediately when the token refreshes.
    const unsubAuth = authEnabled()
      ? useAuthStore.subscribe((state, prev) => {
          if (state.accessToken !== prev.accessToken && state.accessToken) {
            sendReauth(state.accessToken);
          }
        })
      : null;

    return () => {
      // Intentionally bump the LIVE gen so any in-flight reconnect callback self-invalidates — we
      // want the current ref value at teardown time, not a captured snapshot.
      // eslint-disable-next-line react-hooks/exhaustive-deps
      genRef.current++;
      if (reauthInterval) clearInterval(reauthInterval);
      if (unsubAuth) unsubAuth();
      sock.close();
      sockRef.current = null;
      useSessionStore.getState().setConn(sessionId, null);
    };
  }, [spawnId, sessionId, ready]);

  const turn = rt?.turn ?? { state: "idle" as const, queued: 0 };
  const canSend = conn === "connected" && turn.queued < MAX_QUEUED;
  const onSend = (text: string) => sockRef.current?.send(encodePrompt(text));
  const onSetMode = (modeId: string) => sockRef.current?.send(encodeSetMode(modeId));
  const onCancel = () => sockRef.current?.send(encodeCancel());
  // resolve sends the picked optionId (cat H); "" (dismiss) lets the node auto-deny.
  const perm = rt?.perm
    ? { title: rt.perm.title, options: rt.perm.options, resolve: (optionId: string) => {
        sockRef.current?.send(encodePermResponse(rt.perm!.reqId, optionId));
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
      commands={rt?.commands ?? []}
      mode={rt?.mode ?? null}
      onSetMode={onSetMode}
      onCancel={onCancel}
      focusKey={active ? sessionId : null}
    />
  );
}
