/**
 * DER ↔ IEEE-P1363 signature conversion for ECDSA-P256 (WM9).
 *
 * WebCrypto ECDSA uses raw IEEE-P1363 format (r || s, 32 bytes each = 64
 * bytes total for P-256). Go's crypto/ecdsa emits ASN.1-DER. Signatures
 * cross the Go↔TS boundary so both directions are needed.
 */

/** Convert an ASN.1-DER ECDSA signature to IEEE-P1363 (64 bytes for P-256). */
export function derToP1363(der: Uint8Array): Uint8Array {
  let off = 0;
  if (der[off++] !== 0x30) throw new Error("DER: expected SEQUENCE");
  // Length may be 1 or 2 bytes (ASN.1 definite-length encoding)
  let seqLen = der[off++];
  if (seqLen & 0x80) {
    const lenBytes = seqLen & 0x7f;
    seqLen = 0;
    for (let i = 0; i < lenBytes; i++) seqLen = (seqLen << 8) | der[off++];
  }

  if (der[off++] !== 0x02) throw new Error("DER: expected INTEGER r");
  const rLen = der[off++];
  let r = der.subarray(off, off + rLen);
  off += rLen;

  if (der[off++] !== 0x02) throw new Error("DER: expected INTEGER s");
  const sLen = der[off++];
  let s = der.subarray(off, off + sLen);

  // Strip leading 0x00 bytes (ASN.1 uses them for sign, not meaningful for us)
  while (r.length > 32 && r[0] === 0) r = r.subarray(1);
  while (s.length > 32 && s[0] === 0) s = s.subarray(1);
  if (r.length > 32 || s.length > 32) throw new Error("DER: r or s > 32 bytes");

  const p1363 = new Uint8Array(64);
  p1363.set(r, 32 - r.length);
  p1363.set(s, 64 - s.length);
  return p1363;
}

/** Convert an IEEE-P1363 signature (64 bytes for P-256) to ASN.1-DER. */
export function p1363ToDer(p1363: Uint8Array): Uint8Array {
  if (p1363.length !== 64) throw new Error("P1363: expected 64 bytes");
  let r = p1363.subarray(0, 32);
  let s = p1363.subarray(32, 64);

  // Trim leading zeroes (keep at least 1 byte)
  while (r.length > 1 && r[0] === 0) r = r.subarray(1);
  while (s.length > 1 && s[0] === 0) s = s.subarray(1);

  // Prepend 0x00 if high bit set (to keep positive in DER INTEGER)
  const rPad = (r[0] & 0x80) !== 0 ? 1 : 0;
  const sPad = (s[0] & 0x80) !== 0 ? 1 : 0;
  const rLen = r.length + rPad;
  const sLen = s.length + sPad;

  const seqLen = 2 + rLen + 2 + sLen;
  // Handle 2-byte seqLen encoding for pathological cases (very rare for P-256)
  const seqHeader = seqLen > 127 ? [0x30, 0x81, seqLen] : [0x30, seqLen];
  const der = new Uint8Array(seqHeader.length + 2 + rLen + 2 + sLen);
  let off = 0;
  for (const b of seqHeader) der[off++] = b;
  der[off++] = 0x02;
  der[off++] = rLen;
  if (rPad) der[off++] = 0x00;
  der.set(r, off);
  off += r.length;
  der[off++] = 0x02;
  der[off++] = sLen;
  if (sPad) der[off++] = 0x00;
  der.set(s, off);
  return der;
}
