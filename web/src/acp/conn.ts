import type { Message } from "./types";

// WebSocketLike is the subset of WebSocket we use (so tests can fake it).
export interface WebSocketLike {
  binaryType: string;
  onmessage: ((ev: { data: any }) => void) | null;
  send(data: Uint8Array): void;
}

// Conn frames ACP JSON-RPC messages over a WebSocket as newline-delimited JSON.
// Incoming binary/text chunks are buffered and split on "\n"; outgoing messages
// are json+"\n" sent as one binary frame each.
export class Conn {
  private buf = "";
  private dec = new TextDecoder();
  private enc = new TextEncoder();

  constructor(private ws: WebSocketLike, private onMessage: (m: Message) => void) {
    ws.binaryType = "arraybuffer";
    ws.onmessage = (ev) => this.onData(ev.data);
  }

  private onData(data: any) {
    const text =
      typeof data === "string" ? data : this.dec.decode(new Uint8Array(data as ArrayBuffer));
    this.buf += text;
    let i: number;
    while ((i = this.buf.indexOf("\n")) >= 0) {
      const line = this.buf.slice(0, i);
      this.buf = this.buf.slice(i + 1);
      if (line.trim()) this.onMessage(JSON.parse(line) as Message);
    }
  }

  send(m: Message) {
    const obj = { jsonrpc: "2.0", ...m };
    this.ws.send(this.enc.encode(JSON.stringify(obj) + "\n"));
  }
}
