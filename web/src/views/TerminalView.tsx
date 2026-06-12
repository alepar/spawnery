import { useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { ReconnectingSocket } from "@/shell/reconnectingSocket";
import { DEV_TOKEN } from "@/api/spawnlet";
import { cpWsUrl } from "@/config/endpoints";
import { encodeInput, encodeResize } from "@/term/wire";
import { BacklogTracker } from "@/term/backlog";
import { useTermSettings, applyToTerminal } from "@/term/settings";
import { fontById } from "@/term/fonts";

// Per-component client id — TerminalView self-manages its socket (not the App.tsx ACP session).
const CLIENT_ID = (typeof crypto !== "undefined" && crypto.randomUUID)
  ? crypto.randomUUID()
  : `t-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;

/**
 * Derive the primary font family (for `document.fonts.load`) from a fonts.ts stack:
 * the first quoted family (e.g. `"Fira Code"` -> `Fira Code`). The system stack has
 * no quoted custom face — return null so callers skip the load.
 */
function primaryFamily(stack: string): string | null {
  const m = stack.match(/"([^"]+)"/);
  return m ? m[1] : null;
}

export function TerminalView({ spawnId, sessionId = "0", active = true, backlogThreshold = 8 * 1024 * 1024, onConn }: {
  spawnId: string;
  sessionId?: string;
  active?: boolean; // keep-alive: panel is mounted but CSS-hidden when inactive; refit on re-show.
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

  // Appearance settings. The spawnId-keyed socket effect must NOT re-run on settings changes
  // (that would tear down the socket + scrollback), so it reads the latest values via refs —
  // same pattern as onConnRef. A separate [settings, appDark] effect applies live changes.
  const { settings, appDark } = useTermSettings();
  const settingsRef = useRef(settings);
  settingsRef.current = settings;
  const appDarkRef = useRef(appDark);
  appDarkRef.current = appDark;

  // Shared live instances, created by the spawnId effect and consumed by the live-update effect
  // and the keep-alive refit effect.
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const sockRef = useRef<ReconnectingSocket | null>(null);
  // The live-update effect skips its first run: the spawnId effect already applied the initial
  // settings and handles the initial fit/resize on socket open, so the first [settings,appDark]
  // run would only re-do that (and fire a redundant resize before the socket opens).
  const liveInitRef = useRef(false);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;

    const term = new Terminal({ convertEol: false, cursorBlink: true });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    // Apply the chosen appearance from the start (theme/font/size) before the first fit, so cols/rows
    // are measured against the real font metrics. Read via refs since this effect is keyed on spawnId.
    applyToTerminal(term, settingsRef.current, appDarkRef.current);
    termRef.current = term;
    fitRef.current = fit;
    // xterm's FitAddon reads 0x0 under display:none -> throws / Infinity resize loop. Only ever fit
    // while the host is actually visible (offsetParent is null when display:none).
    const safeFit = () => { if (host.offsetParent !== null) { try { fit.fit(); } catch { /* hidden race */ } } };
    safeFit();
    // Focus the terminal as soon as the spawn is (re)selected — the effect is keyed on spawnId, so
    // this runs on mount and on every spawn switch — so keystrokes go straight into the TUI, matching
    // how a web-native spawn auto-focuses its chat input on selection.
    term.focus();

    const backlog = new BacklogTracker(backlogThreshold);

    // Intentional teardown (unmount / spawn switch) closes the socket, which fires onDown. That
    // async "reconnecting" would land AFTER selectSpawn already set the next spawn's dot and clobber
    // it (onConn isn't spawn-scoped). Guard the down callback so an intentional close stays silent.
    let closing = false;

    onConnRef.current?.("connecting");
    const sock = new ReconnectingSocket(cpWsUrl("/ws/session"), {
      onOpen: () => {
        sock.send(JSON.stringify({ spawnId, sessionId, clientId: CLIENT_ID, token: DEV_TOKEN, cursor: 0 }));
        safeFit();
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
    sockRef.current = sock;

    const onData = term.onData((d) => sock.send(encodeInput(d)));
    const onResize = term.onResize(({ cols, rows }) => sock.send(encodeResize(cols, rows)));
    const ro = new ResizeObserver(() => safeFit());
    ro.observe(host);

    return () => {
      closing = true; // intentional teardown -> suppress the close-driven onDown "reconnecting"
      ro.disconnect();
      onData.dispose();
      onResize.dispose();
      sock.close();
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
      sockRef.current = null;
    };
  }, [spawnId, sessionId]);

  // Keep-alive: when an inactive (display:none) panel becomes active again, the host regained layout
  // — refit to the new size and refocus. Never fit while hidden (handled by safeFit, but the guard
  // here also avoids a wasted rAF). rAF lets the browser apply `display:block` before measuring.
  useEffect(() => {
    if (!active) return;
    const id = requestAnimationFrame(() => {
      const host = hostRef.current;
      if (host && host.offsetParent !== null) { try { fitRef.current?.fit(); } catch { /* race */ } }
      termRef.current?.focus();
    });
    return () => cancelAnimationFrame(id);
  }, [active]);

  // Live appearance updates: apply theme/font/size to the EXISTING terminal (no teardown, so
  // scrollback is preserved). Font/size changes alter cell metrics → re-fit and re-send the resize
  // so the PTY follows. Keyed on [settings, appDark] only; reaches the live instances via refs.
  useEffect(() => {
    // Skip the initial run (covered by the spawnId effect's construct + onOpen sizing).
    if (!liveInitRef.current) { liveInitRef.current = true; return; }

    const term = termRef.current;
    const fit = fitRef.current;
    if (!term || !fit) return; // socket effect hasn't created the terminal yet

    let cancelled = false;
    applyToTerminal(term, settings, appDark);

    const finishLayout = () => {
      if (cancelled || termRef.current !== term) return; // unmounted / spawn switched mid-await
      fit.fit();
      term.refresh(0, term.rows - 1);
      sockRef.current?.send(encodeResize(term.cols, term.rows));
    };

    const family = primaryFamily(fontById(settings.fontFamily).stack);
    if (family) {
      // Ensure the new face/size is loaded before measuring cell metrics.
      void document.fonts.load(`${settings.fontSize}px "${family}"`).then(finishLayout, finishLayout);
    } else {
      // System stack: no custom face to load — lay out synchronously.
      finishLayout();
    }

    return () => { cancelled = true; };
  }, [settings, appDark]);

  return <div data-testid="terminal-view" ref={hostRef} className="h-full w-full" />;
}
