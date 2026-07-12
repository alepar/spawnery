import { useEffect, useRef } from "react";
import { Conn } from "@/acp/conn";
import { encodePrompt, encodePermResponse, encodeSetMode, encodeCancel, type Frame } from "@/acp/frames";
import { ReconnectingSocket } from "@/shell/reconnectingSocket";
import { getAccessToken, authEnabled, useSessionStore as useAuthStore } from "@/auth/session";
import { buildSessionBindFrame } from "@/auth/sessionBind";
import { buildNodeReauthControl, type VerifiedSessionAuthorization } from "@/auth/sessionReauth";
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
  const sendRef = useRef<(data: string | Uint8Array) => void>(() => {});
  const genRef = useRef(0);

  useEffect(() => {
    // Not ready yet: don't open the socket. Show an honest grey-pulse "waiting" dot and bail; the
    // effect re-runs (keyed on `ready`) and opens the socket the instant the session flips ready.
    if (!ready) {
      useSessionStore.getState().setConn(sessionId, "waiting");
      return;
    }
    const gen = ++genRef.current;
    let attachmentSequence = 0;
    let nodeReauthSequence = 0;
    let authorization: VerifiedSessionAuthorization | null = null;
    let bound = false;
    let binding = false;
    let pendingPairReauth = false;
    let pendingSends: Array<string | Uint8Array> = [];
    let pendingBytes = 0;
    const clearPendingSends = () => { pendingSends = []; pendingBytes = 0; };
    const sendSize = (data: string | Uint8Array) => typeof data === "string" ? new TextEncoder().encode(data).byteLength : data.byteLength;
    const sendWhenBound = (data: string | Uint8Array) => {
      if (bound) { sock.send(data); return; }
      if (!binding) return;
      const size = sendSize(data);
      if (pendingSends.length >= 256 || pendingBytes + size > 256 * 1024) return;
      pendingSends.push(data);
      pendingBytes += size;
    };
    sendRef.current = sendWhenBound;
    useSessionStore.getState().setConn(sessionId, "connecting");
    const sock = new ReconnectingSocket(cpWsUrl("/ws/session"), {
      onOpen: async () => {
        if (genRef.current !== gen) return;
        const openSequence = ++attachmentSequence;
        nodeReauthSequence++;
        authorization = null;
        bound = false;
        binding = true;
        clearPendingSends();
        pendingPairReauth = false;
        // Fresh frame receiver per (re)connect; wire it BEFORE the bind so replay can't precede onmessage.
        new Conn(sock, (m) => { if (genRef.current === gen) useSessionStore.getState().applyFrame(sessionId, m as Frame); });
        const cursor = useSessionStore.getState().acp[sessionId]?.lastSeq ?? 0;
        // Bind frame carries the session-open SignedIntent the enforced node requires (else
        // MISSING_INTENT NACK -> client never attaches -> blank panel).
        try {
          const built = await buildSessionBindFrame(spawnId, sessionId, CLIENT_ID, cursor, openSequence);
          if (genRef.current !== gen || openSequence !== attachmentSequence) return;
          const { authorization: verified, ...frame } = built;
          if (authEnabled() && !verified) throw new Error("session bind: authorization context missing");
          authorization = verified ?? null;
          sock.send(JSON.stringify(frame));
          bound = true;
          binding = false;
          for (const pending of pendingSends) sock.send(pending);
          clearPendingSends();
          if (pendingPairReauth) {
            pendingPairReauth = false;
            const latest = useAuthStore.getState();
            if (latest.cpAccessToken && latest.nodeAccessToken) {
              sendReauth(latest.cpAccessToken);
              sendNodeReauth(latest.nodeAccessToken);
            }
          }
          useSessionStore.getState().setConn(sessionId, "connected");
        } catch {
          binding = false;
          clearPendingSends();
          if (genRef.current === gen && openSequence === attachmentSequence) sock.close();
        }
      },
      onDown: () => {
        bound = false;
        binding = false;
        clearPendingSends();
        if (genRef.current === gen) useSessionStore.getState().setConn(sessionId, "reconnecting");
      },
    });
    sockRef.current = sock;

    // In-band reauth: ~14min interval (under the 15min ws.go deadline).
    const REAUTH_MS = 14 * 60 * 1000;
    const sendReauth = (token: string) => {
      if (!bound) return;
      try { sock.send(JSON.stringify({ type: "reauth", token })); } catch { sock.close(); }
    };
    const sendNodeReauth = (nodeAccessToken: string) => {
      if (!bound) return;
      const verified = authorization;
      if (!verified) return;
      const reauthSequence = ++nodeReauthSequence;
      void buildNodeReauthControl(verified, nodeAccessToken).then((control) => {
        if (genRef.current !== gen || reauthSequence !== nodeReauthSequence ||
            verified.attachmentSequence !== attachmentSequence) return;
        sock.send(JSON.stringify(control));
      }).catch(() => {
        if (genRef.current === gen && reauthSequence === nodeReauthSequence &&
            verified.attachmentSequence === attachmentSequence) sock.close();
      });
    };
    const reauthInterval = authEnabled()
      ? setInterval(() => sendReauth(getAccessToken()), REAUTH_MS)
      : null;

    // Push a reauth frame immediately when the token refreshes.
    const unsubAuth = authEnabled()
      ? useAuthStore.subscribe((state, prev) => {
          const cpChanged = state.cpAccessToken !== prev.cpAccessToken && !!state.cpAccessToken;
          const nodeChanged = state.nodeAccessToken !== prev.nodeAccessToken && !!state.nodeAccessToken;
          if (!bound && (cpChanged || nodeChanged)) {
            pendingPairReauth = true;
            return;
          }
          if (cpChanged) {
            sendReauth(state.cpAccessToken);
          }
          if (nodeChanged) {
            sendNodeReauth(state.nodeAccessToken);
          }
        })
      : null;

    return () => {
      // Intentionally bump the LIVE gen so any in-flight reconnect callback self-invalidates — we
      // want the current ref value at teardown time, not a captured snapshot.
      // eslint-disable-next-line react-hooks/exhaustive-deps
      genRef.current++;
      sendRef.current = () => {};
      binding = false;
      clearPendingSends();
      attachmentSequence++;
      nodeReauthSequence++;
      if (reauthInterval) clearInterval(reauthInterval);
      if (unsubAuth) unsubAuth();
      sock.close();
      sockRef.current = null;
      useSessionStore.getState().setConn(sessionId, null);
    };
  }, [spawnId, sessionId, ready]);

  const turn = rt?.turn ?? { state: "idle" as const, queued: 0 };
  const canSend = conn === "connected" && turn.queued < MAX_QUEUED;
  const onSend = (text: string) => sendRef.current(encodePrompt(text));
  const onSetMode = (modeId: string) => sendRef.current(encodeSetMode(modeId));
  const onCancel = () => sendRef.current(encodeCancel());
  // resolve sends the picked optionId (cat H); "" (dismiss) lets the node auto-deny.
  const perm = rt?.perm
    ? { title: rt.perm.title, options: rt.perm.options, resolve: (optionId: string) => {
        sendRef.current(encodePermResponse(rt.perm!.reqId, optionId));
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
