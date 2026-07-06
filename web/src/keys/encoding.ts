/**
 * Encoding utilities for the owner-sealed-secrets layer.
 *
 * Sourced from @spawnery/client — see sdk/ts/src/keys/encoding.ts for implementation notes.
 * encodeFields is the load-bearing cross-language interop primitive; verified against the Go
 * golden vectors in deviceset.test.ts.
 */
export {
  encodeFields,
  sha256,
  toBase64,
  fromBase64,
  bytesEqual,
  parseBigIntNanos,
} from "@spawnery/client";
