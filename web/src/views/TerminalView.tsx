import { useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { ReconnectingSocket } from "@/shell/reconnectingSocket";
import { DEV_TOKEN } from "@/api/spawnlet";
import { encodeInput, encodeResize } from "@/term/wire";

// Per-component client id — TerminalView self-manages its socket (not the App.tsx ACP session).
const CLIENT_ID = (typeof crypto !== "undefined" && crypto.randomUUID)
  ? crypto.randomUUID()
  : `t-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;

export function TerminalView({ spawnId }: { spawnId: string }) {
  const hostRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;

    const term = new Terminal({ convertEol: false, fontFamily: "monospace", cursorBlink: true });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();

    const sock = new ReconnectingSocket(`ws://${location.host}/ws/session`, {
      onOpen: () => {
        sock.send(JSON.stringify({ spawnId, clientId: CLIENT_ID, token: DEV_TOKEN, cursor: 0 }));
        fit.fit();
        sock.send(encodeResize(term.cols, term.rows));
      },
      onDown: () => {},
      onMessage: (data: ArrayBuffer | string) => {
        term.write(typeof data === "string" ? data : new Uint8Array(data));
      },
    });

    // Set binary type so received messages arrive as ArrayBuffer
    sock.binaryType = "arraybuffer";

    const onData = term.onData((d) => sock.send(encodeInput(d)));
    const onResize = term.onResize(({ cols, rows }) => sock.send(encodeResize(cols, rows)));
    const ro = new ResizeObserver(() => fit.fit());
    ro.observe(host);

    return () => {
      ro.disconnect();
      onData.dispose();
      onResize.dispose();
      sock.close();
      term.dispose();
    };
  }, [spawnId]);

  return <div data-testid="terminal-view" ref={hostRef} className="h-full w-full" />;
}
