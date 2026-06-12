/**
 * Encoding utilities for the owner-sealed-secrets layer.
 *
 * encodeFields must match Go's seal.encodeFields exactly: each part is prefixed
 * with an 8-byte big-endian uint64 length so distinct field tuples never collide.
 * This function is the load-bearing cross-language interop primitive — it is
 * verified against the Go golden vectors in deviceset.test.ts.
 */

/** encodeFields produces an unambiguous length-prefixed concatenation (WM9). */
export function encodeFields(...parts: Uint8Array[]): Uint8Array {
  let total = 0;
  for (const p of parts) total += 8 + p.length;
  const out = new Uint8Array(total);
  let offset = 0;
  const view = new DataView(out.buffer);
  for (const p of parts) {
    view.setBigUint64(offset, BigInt(p.length), false); // big-endian
    offset += 8;
    out.set(p, offset);
    offset += p.length;
  }
  return out;
}

/** sha256 wraps crypto.subtle.digest for convenience. */
export async function sha256(data: Uint8Array): Promise<Uint8Array> {
  // TS 5.9: Uint8Array<ArrayBufferLike> is not assignable to BufferSource; cast is safe.
  const digest = await crypto.subtle.digest("SHA-256", data as unknown as Uint8Array<ArrayBuffer>);
  return new Uint8Array(digest as ArrayBuffer);
}

/** Encode a Uint8Array to standard base64. */
export function toBase64(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}

/** Decode standard base64 to Uint8Array. */
export function fromBase64(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** Constant-time byte comparison (avoids timing side-channels in verification). */
export function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i];
  return diff === 0;
}

/**
 * parseBigIntNanos parses a decimal u64 nanosecond string with BigInt precision
 * (WM10: uint64 UnixNano exceeds Number.MAX_SAFE_INTEGER; never parse via Date
 * or Number).
 */
export function parseBigIntNanos(s: string): bigint {
  if (!/^\d+$/.test(s)) throw new Error(`invalid u64 nanos string: ${s}`);
  return BigInt(s);
}
