/**
 * BIP-39 seed derivation for the owner-sealed-secrets layer.
 *
 * Implements two operations that must match the Go side
 * (github.com/tyler-smith/go-bip39):
 *   1. mnemonicToSeed(mnemonic, passphrase) → 64-byte seed
 *      = PBKDF2(SHA-512, mnemonic, "mnemonic" + passphrase, 2048 iterations)
 *   2. generateMnemonic() → 24-word BIP-39 phrase (256-bit entropy)
 *
 * Validation uses the English BIP-39 word list (2048 words). The word list
 * is embedded at the bottom of this file to avoid a network fetch.
 *
 * Note (WL1): mnemonic-derived keys are extractable-by-construction while in
 * use — the seed passes through zeroable ArrayBuffers and is imported
 * non-extractable immediately afterwards. Mnemonic inputs disable autocomplete.
 */

import { bip39Words } from "./bip39Words";

/** Validate that all words in the phrase belong to the BIP-39 word list. */
export function isMnemonicValid(mnemonic: string): boolean {
  const words = mnemonic.trim().split(/\s+/);
  if (words.length !== 24) return false;
  const wordSet = new Set(bip39Words);
  return words.every((w) => wordSet.has(w));
}

/**
 * mnemonicToSeed derives the 64-byte BIP-39 seed from a mnemonic phrase
 * and optional passphrase, matching Go's bip39.NewSeed.
 *
 * PBKDF2(SHA-512, mnemonic_bytes, "mnemonic" + passphrase, 2048, 64)
 */
export async function mnemonicToSeed(
  mnemonic: string,
  passphrase = "",
): Promise<Uint8Array> {
  const enc = new TextEncoder();
  const mnemonicBytes = enc.encode(mnemonic.normalize("NFKD"));
  const saltBytes = enc.encode("mnemonic" + passphrase.normalize("NFKD"));

  const baseKey = await crypto.subtle.importKey(
    "raw",
    mnemonicBytes,
    "PBKDF2",
    false,
    ["deriveBits"],
  );
  const bits = await crypto.subtle.deriveBits(
    {
      name: "PBKDF2",
      salt: saltBytes,
      iterations: 2048,
      hash: "SHA-512",
    },
    baseKey,
    512, // 64 bytes
  );
  return new Uint8Array(bits);
}

/**
 * generateMnemonic generates a fresh 24-word BIP-39 mnemonic (256-bit
 * entropy). Returns the space-separated word phrase.
 *
 * Algorithm (BIP-39):
 *   1. Generate 32 bytes (256 bits) of entropy.
 *   2. Compute SHA-256(entropy); take the first 8 bits as checksum.
 *   3. Concatenate entropy bits + checksum = 264 bits.
 *   4. Split into 24 groups of 11 bits; each maps to a word.
 */
export async function generateMnemonic(): Promise<string> {
  const entropy = crypto.getRandomValues(new Uint8Array(32));
  return entropyToMnemonic(entropy);
}

/** entropyToMnemonic converts 32 raw entropy bytes to a 24-word BIP-39 phrase. */
export async function entropyToMnemonic(entropy: Uint8Array): Promise<string> {
  if (entropy.length !== 32) throw new Error("BIP-39: entropy must be 32 bytes");
  // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
  const hashBuf = await crypto.subtle.digest("SHA-256", entropy as unknown as Uint8Array<ArrayBuffer>);
  const checkByte = new Uint8Array(hashBuf)[0]; // first 8 bits of hash

  // Build a 264-bit (33-byte) buffer: entropy || checksum
  const bits = new Uint8Array(33);
  bits.set(entropy);
  bits[32] = checkByte;

  // Extract 24 groups of 11 bits
  const words: string[] = [];
  for (let i = 0; i < 24; i++) {
    const bitOff = i * 11;
    const byteOff = bitOff >> 3;
    const bitShift = bitOff & 7;
    // Read 3 bytes starting at byteOff (may span boundaries)
    const b0 = bits[byteOff] ?? 0;
    const b1 = bits[byteOff + 1] ?? 0;
    const b2 = bits[byteOff + 2] ?? 0;
    const combined = (b0 << 16) | (b1 << 8) | b2;
    const idx = (combined >> (13 - bitShift)) & 0x7ff;
    words.push(bip39Words[idx]);
  }
  return words.join(" ");
}
