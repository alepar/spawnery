// WebSocketLike is the subset of WebSocket we use (so tests can fake it).
export interface WebSocketLike {
  binaryType: string;
  onmessage: ((ev: { data: any }) => void) | null;
}

// Conn is a receive-only reader: it splits the WebSocket's binary/text stream into newline-delimited
// JSON frames and hands each parsed object to onFrame. Sending is done by the caller over the raw
// socket (encodePrompt / encodePermResponse in frames.ts) — Conn never writes.
export class Conn {
  private buf = "";
  private dec = new TextDecoder();

  constructor(ws: WebSocketLike, private onFrame: (m: unknown) => void) {
    ws.binaryType = "arraybuffer";
    ws.onmessage = (ev) => this.onData(ev.data);
  }

  private onData(data: any) {
    // stream:true keeps a multi-byte UTF-8 char that's split across frames intact.
    const text =
      typeof data === "string"
        ? data
        : this.dec.decode(new Uint8Array(data as ArrayBuffer), { stream: true });
    this.buf += text;
    let i: number;
    while ((i = this.buf.indexOf("\n")) >= 0) {
      const line = this.buf.slice(0, i);
      this.buf = this.buf.slice(i + 1);
      if (!line.trim()) continue;
      try {
        this.onFrame(JSON.parse(line));
      } catch (e) {
        console.error("acp: bad frame", e, line);
      }
    }
  }
}
