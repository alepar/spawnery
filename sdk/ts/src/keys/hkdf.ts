/**
 * HKDF-Expand-only, matching Go's crypto/hkdf.Expand.
 *
 * Go's hkdf.Expand(hash, prk, info, length) performs HKDF-Expand (RFC 5869
 * §2.3) directly without an extraction step — it treats the input as the PRK.
 * WebCrypto provides only the full HKDF (extract+expand) via importKey/deriveKey
 * and does not expose HKDF-Expand-only. We implement it manually using HMAC.
 *
 * T(0) = empty
 * T(i) = HMAC-SHA256(prk, T(i-1) || info || i)
 * OKM  = T(1) || T(2) || … (first `length` bytes)
 */
export async function hkdfExpand(
  prk: Uint8Array,
  info: string,
  length: number,
): Promise<Uint8Array> {
  const prkKey = await crypto.subtle.importKey(
    "raw",
    prk as unknown as Uint8Array<ArrayBuffer>, // TS 5.9 BufferSource cast
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const infoBytes = new TextEncoder().encode(info);
  const blocks = Math.ceil(length / 32);
  const output = new Uint8Array(length);
  let prev = new Uint8Array(0);
  let outputOffset = 0;

  for (let i = 1; i <= blocks; i++) {
    const data = new Uint8Array(prev.length + infoBytes.length + 1);
    data.set(prev, 0);
    data.set(infoBytes, prev.length);
    data[prev.length + infoBytes.length] = i;
    prev = new Uint8Array(await crypto.subtle.sign("HMAC", prkKey, data));
    const take = Math.min(32, length - outputOffset);
    output.set(prev.subarray(0, take), outputOffset);
    outputOffset += take;
  }
  return output;
}
