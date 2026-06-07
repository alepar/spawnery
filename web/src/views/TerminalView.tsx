import { useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { ReconnectingSocket } from "@/shell/reconnectingSocket";
import { DEV_TOKEN } from "@/api/spawnlet";
import { encodeInput, encodeResize } from "@/term/wire";
import { BacklogTracker } from "@/term/backlog";

// Per-component client id — TerminalView self-manages its socket (not the App.tsx ACP session).
const CLIENT_ID = (typeof crypto !== "undefined" && crypto.randomUUID)
  ? crypto.randomUUID()
  : `t-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;

export function TerminalView({ spawnId, backlogThreshold = 8 * 1024 * 1024, onConn }: {
  spawnId: string;
  backlogThreshold?: number;
  // Reports the terminal's own socket state up to the chat-header ConnStatus dot. tmux spawns
  // self-manage this socket (App.tsx gates openSession off for them), so without this the dot
  // would stay on the App's null/waiting state (sp-9xr.18).
  onConn?: (s: "connecting" | "connected" | "reconnecting") => void;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  // Keep the latest onConn in a ref so the socket callbacks don't pin a stale closure (the effect
  // is keyed on spawnId only, to avoid tearing down/reopening the socket when the parent re-renders).
  const onConnRef = useRef(onConn);
  onConnRef.current = onConn;

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;

    const term = new Terminal({ convertEol: false, fontFamily: "monospace", cursorBlink: true });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();

    const backlog = new BacklogTracker(backlogThreshold);

    // Intentional teardown (unmount / spawn switch) closes the socket, which fires onDown. That
    // async "reconnecting" would land AFTER selectSpawn already set the next spawn's dot and clobber
    // it (onConn isn't spawn-scoped). Guard the down callback so an intentional close stays silent.
    let closing = false;

    onConnRef.current?.("connecting");
    const sock = new ReconnectingSocket(`ws://${location.host}/ws/session`, {
      onOpen: () => {
        sock.send(JSON.stringify({ spawnId, clientId: CLIENT_ID, token: DEV_TOKEN, cursor: 0 }));
        fit.fit();
        sock.send(encodeResize(term.cols, term.rows));
        onConnRef.current?.("connected");
      },
      onDown: () => { if (!closing) onConnRef.current?.("reconnecting"); },
      onMessage: (data: ArrayBuffer | string) => {
        const bytes = typeof data === "string" ? new TextEncoder().encode(data) : new Uint8Array(data);
        const n = bytes.length;
        const before = backlog.wedges;
        backlog.onWrite(n);
        term.write(bytes, () => backlog.onAck(n));
        if (backlog.wedges > before) {
          console.warn(`terminal backlog wedge: spawn=${spawnId} outstanding=${backlog.outstanding}B wedges=${backlog.wedges}`);
        }
      },
    });

    // Set binary type so received messages arrive as ArrayBuffer
    sock.binaryType = "arraybuffer";

    const onData = term.onData((d) => sock.send(encodeInput(d)));
    const onResize = term.onResize(({ cols, rows }) => sock.send(encodeResize(cols, rows)));
    const ro = new ResizeObserver(() => fit.fit());
    ro.observe(host);

    return () => {
      closing = true; // intentional teardown -> suppress the close-driven onDown "reconnecting"
      ro.disconnect();
      onData.dispose();
      onResize.dispose();
      sock.close();
      term.dispose();
    };
  }, [spawnId]);

  return <div data-testid="terminal-view" ref={hostRef} className="h-full w-full" />;
}
