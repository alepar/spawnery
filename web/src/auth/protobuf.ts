/**
 * Minimal deterministic protobuf writer + reader (proto3 wire format).
 *
 * Wire types: 0 = varint, 2 = length-delimited (string, bytes, embedded message).
 * Omit-zero: proto3 default → fields with zero/empty values are not written.
 * Field ordering: fields written in field-number order (required for determinism + golden vectors).
 *
 * Covers IntentBody (fields 1–12) and SessionTokenBody read (f1,f2,f6,f7).
 * NOT a general-purpose codec — only the field types actually used here.
 */

// ── Varint encoding/decoding ──────────────────────────────────────────────────

/** Encode a non-negative integer as a protobuf varint (LEB128). */
export function encodeVarint(v: number | bigint): Uint8Array {
  let n = typeof v === "bigint" ? v : BigInt(v);
  const out: number[] = [];
  do {
    let b = Number(n & 0x7fn);
    n >>= 7n;
    if (n !== 0n) b |= 0x80;
    out.push(b);
  } while (n !== 0n);
  return new Uint8Array(out);
}

/** Decode a protobuf varint from buf at offset. Returns [value, nextOffset]. */
export function decodeVarint(buf: Uint8Array, off: number): [bigint, number] {
  let result = 0n;
  let shift = 0n;
  while (off < buf.length) {
    const b = buf[off++];
    result |= BigInt(b & 0x7f) << shift;
    shift += 7n;
    if ((b & 0x80) === 0) break;
  }
  return [result, off];
}

// ── Field tag ────────────────────────────────────────────────────────────────

/** Encode field tag = (fieldNumber << 3) | wireType. */
function fieldTag(fieldNumber: number, wireType: number): Uint8Array {
  return encodeVarint((fieldNumber << 3) | wireType);
}

// ── ProtoWriter: deterministic, field-number-ordered writer ──────────────────

export class ProtoWriter {
  // Store entries as {fieldNum, bytes} so we can sort by field number before emitting.
  private entries: Array<{ n: number; bytes: Uint8Array }> = [];

  /** Write a varint field (wire type 0). Omits zero. */
  writeVarint(fieldNumber: number, value: number | bigint): void {
    const v = typeof value === "bigint" ? value : BigInt(value);
    if (v === 0n) return; // proto3 omit-zero
    const tag = fieldTag(fieldNumber, 0);
    const val = encodeVarint(v);
    const combined = new Uint8Array(tag.length + val.length);
    combined.set(tag);
    combined.set(val, tag.length);
    this.entries.push({ n: fieldNumber, bytes: combined });
  }

  /** Write a length-delimited field (wire type 2) for string or bytes. Omits empty. */
  writeBytes(fieldNumber: number, value: Uint8Array | string): void {
    let data: Uint8Array;
    if (typeof value === "string") {
      data = new TextEncoder().encode(value);
    } else {
      data = value;
    }
    if (data.length === 0) return; // proto3 omit-empty
    const tag = fieldTag(fieldNumber, 2);
    const lenPrefix = encodeVarint(data.length);
    const combined = new Uint8Array(tag.length + lenPrefix.length + data.length);
    combined.set(tag);
    combined.set(lenPrefix, tag.length);
    combined.set(data, tag.length + lenPrefix.length);
    this.entries.push({ n: fieldNumber, bytes: combined });
  }

  /** Write an embedded message field (wire type 2). Omits if empty. */
  writeMessage(fieldNumber: number, writer: ProtoWriter): void {
    const inner = writer.finish();
    if (inner.length === 0) return;
    this.writeBytes(fieldNumber, inner);
  }

  /** Serialize all fields in field-number order. */
  finish(): Uint8Array {
    // Sort ascending by field number (proto3 canonical ordering).
    this.entries.sort((a, b) => a.n - b.n);
    let total = 0;
    for (const e of this.entries) total += e.bytes.length;
    const out = new Uint8Array(total);
    let off = 0;
    for (const e of this.entries) {
      out.set(e.bytes, off);
      off += e.bytes.length;
    }
    return out;
  }
}

// ── ProtoReader: sequential field reader ─────────────────────────────────────

export interface ProtoField {
  fieldNumber: number;
  wireType: number;
  /** Varint value (wire type 0). */
  varint?: bigint;
  /** Raw bytes (wire type 2: string, bytes, or embedded message). */
  bytes?: Uint8Array;
}

/** Read all top-level fields from buf, returning them in wire order. */
export function readFields(buf: Uint8Array): ProtoField[] {
  const fields: ProtoField[] = [];
  let off = 0;
  while (off < buf.length) {
    let tag: bigint;
    [tag, off] = decodeVarint(buf, off);
    const fieldNumber = Number(tag >> 3n);
    const wireType = Number(tag & 0x7n);
    if (wireType === 0) {
      let val: bigint;
      [val, off] = decodeVarint(buf, off);
      fields.push({ fieldNumber, wireType, varint: val });
    } else if (wireType === 2) {
      let len: bigint;
      [len, off] = decodeVarint(buf, off);
      const n = Number(len);
      fields.push({ fieldNumber, wireType, bytes: buf.slice(off, off + n) });
      off += n;
    } else {
      // Unknown wire type — skip this and remaining bytes (malformed or unsupported).
      break;
    }
  }
  return fields;
}
