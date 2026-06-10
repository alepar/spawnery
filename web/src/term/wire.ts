// Client→node tmux frame opcodes (must match internal/node/tmuxrelay.go).
export const TMUX_OP_INPUT = 0x00;
export const TMUX_OP_RESIZE = 0x01;

export function encodeInput(data: string): Uint8Array {
  const body = new TextEncoder().encode(data);
  const out = new Uint8Array(body.length + 1);
  out[0] = TMUX_OP_INPUT;
  out.set(body, 1);
  return out;
}

export function encodeResize(cols: number, rows: number): Uint8Array {
  const body = new TextEncoder().encode(`${cols} ${rows}`);
  const out = new Uint8Array(body.length + 1);
  out[0] = TMUX_OP_RESIZE;
  out.set(body, 1);
  return out;
}
