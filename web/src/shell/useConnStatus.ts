import { useCallback, useEffect, useRef, useState } from "react";

export type ConnState = "waiting" | "connecting" | "slow" | "connected" | "error";

// useConnStatus is the WS connection-status state machine for the chat-header indicator. `conn` is
// null when there is no live socket (the indicator is hidden). connecting() arms a `slowMs` timer
// that flips connecting -> slow if the socket is still connecting when it fires.
export function useConnStatus(slowMs = 5000) {
  const [conn, setConn] = useState<ConnState | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const clearTimer = useCallback(() => {
    if (timer.current) {
      clearTimeout(timer.current);
      timer.current = null;
    }
  }, []);

  const connecting = useCallback(() => {
    clearTimer();
    setConn("connecting");
    timer.current = setTimeout(() => {
      setConn((c) => (c === "connecting" ? "slow" : c));
    }, slowMs);
  }, [clearTimer, slowMs]);

  // The selected spawn is still starting (no socket yet) — grey-pulse "waiting" until it goes active.
  const waiting = useCallback(() => { clearTimer(); setConn("waiting"); }, [clearTimer]);
  const connected = useCallback(() => { clearTimer(); setConn("connected"); }, [clearTimer]);
  const errored = useCallback(() => { clearTimer(); setConn("error"); }, [clearTimer]);
  // Unexpected ws close: keep the red error if we were errored, else hide.
  const closed = useCallback(() => { clearTimer(); setConn((c) => (c === "error" ? c : null)); }, [clearTimer]);
  // Intentional teardown (switch away / suspend / stop): always hide.
  const reset = useCallback(() => { clearTimer(); setConn(null); }, [clearTimer]);

  useEffect(() => clearTimer, [clearTimer]); // clear the timer on unmount

  return { conn, connecting, connected, errored, closed, reset, waiting };
}
