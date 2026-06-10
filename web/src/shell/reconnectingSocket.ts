import { WebSocket as PartySocket } from "partysocket";
import type { WebSocketLike } from "@/acp/conn";

// SCHEDULE: per-attempt connect-timeout (ms); the last value repeats forever; a successful connect
// resets to SCHEDULE[0]. NOTE: partysocket re-dials inside _handleClose (reading connectionTimeout)
// BEFORE dispatching the `close` event we hook, so the *effective* initial-connect sequence repeats
// 500 once (500,500,2500,5000,15000,30000,…); post-success recovery is exact. Accepted (see spec).
export const SCHEDULE = [500, 2500, 5000, 15000, 30000];

// The subset of a PartySocket instance the wrapper drives. `_options` is partysocket's soft-private
// options bag; we mutate `connectionTimeout` on it — the only internals dependency, guarded by the
// pinned version + the structural test in reconnectingSocket.test.ts.
export interface PartySocketLike {
  binaryType: string;
  onmessage: ((ev: { data: any }) => void) | null;
  send(data: any): void;
  close(): void;
  addEventListener(type: "open" | "close" | "message", cb: (ev: any) => void): void;
  _options: { connectionTimeout?: number };
}

export type MakeSocket = (url: string, options: Record<string, any>) => PartySocketLike;

const defaultMake: MakeSocket = (url, options) =>
  new PartySocket(url, [], options) as unknown as PartySocketLike;

export interface ReconnectingOpts {
  onOpen: () => void;       // fires on EVERY (re)connection — caller re-runs the ACP handshake
  onDown: () => void;       // fires on every failed attempt / drop
  onMessage?: (data: ArrayBuffer | string) => void;  // optional raw message handler (bypasses Conn/NDJSON)
  makeSocket?: MakeSocket;  // injected in tests
}

// ReconnectingSocket wraps a PartySocket as the WebSocketLike that acp/Conn consumes. It reconnects
// forever; each attempt's connect-timeout walks SCHEDULE (escalate on close, reset on open). The
// reconnection *delay* is pinned near-zero so the escalating connect-timeout is the sole pacing.
export class ReconnectingSocket implements WebSocketLike {
  private ps: PartySocketLike;
  private step = 0;
  onmessage: ((ev: { data: any }) => void) | null = null;

  constructor(url: string, opts: ReconnectingOpts) {
    const make = opts.makeSocket ?? defaultMake;
    this.ps = make(url, {
      connectionTimeout: SCHEDULE[0],
      minReconnectionDelay: 50,
      maxReconnectionDelay: 50,
      maxRetries: Infinity,
    });
    this.ps.addEventListener("message", (ev) => {
      this.onmessage?.(ev);
      opts.onMessage?.(ev.data);
    });
    this.ps.addEventListener("open", () => {
      this.step = 0;
      this.ps._options.connectionTimeout = SCHEDULE[0]; // success resets the chain
      opts.onOpen();
    });
    this.ps.addEventListener("close", () => {
      this.step = Math.min(this.step + 1, SCHEDULE.length - 1);
      this.ps._options.connectionTimeout = SCHEDULE[this.step]; // escalate the next attempt
      opts.onDown();
    });
  }

  get binaryType() { return this.ps.binaryType; }
  set binaryType(v: string) { this.ps.binaryType = v; }
  send(data: string | ArrayBufferView) { this.ps.send(data); }
  close() { this.ps.close(); }
}
