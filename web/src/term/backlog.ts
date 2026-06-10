// BacklogTracker counts how often the unparsed-byte backlog crosses a threshold (a proxy for the
// browser terminal "wedging" — xterm.js parses ~5–35 MB/s with a ~50 MB write-buffer cap). It does
// NOT throttle; it only observes. A full ACK/credit scheme is deferred until this metric shows it's
// needed (sp-9xr.11 punt). onWrite when bytes are handed to term.write; onAck in term.write's callback.
export class BacklogTracker {
  outstanding = 0;
  wedges = 0;
  private over = false;
  constructor(private threshold: number) {}
  onWrite(n: number) {
    this.outstanding += n;
    if (!this.over && this.outstanding >= this.threshold) {
      this.over = true;
      this.wedges++;
    }
  }
  onAck(n: number) {
    this.outstanding = Math.max(0, this.outstanding - n);
    if (this.over && this.outstanding < this.threshold) this.over = false;
  }
}
