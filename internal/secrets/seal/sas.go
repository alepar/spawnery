package seal

import (
	"crypto/sha256"
	"fmt"
)

const sasMagic = "sas/v1"

// DeriveSAS computes the enrollment Short Authentication String (SAS) from the
// chain state and the new device's public keys.
//
// Both the approver and the enrollee independently compute this code; a human
// confirms the two codes match out-of-band before the approver commits the
// add-entry (spec §2 [WM4]). The code is NEVER parsed from the enrollment
// link — a link-carried code authenticates nothing.
//
// Formula:
//
//	SHA-256(encodeFields("sas/v1", genesis_hash, head_hash, new_x25519_pub, new_sign_pub))
//
// Output: first 6 bytes of the digest (48 bits) encoded as base-36
// (lowercase alphanum), chunked as "xxxx-xxxx-xxxx" for readability.
//
// Cross-language note: must match web/src/keys/sas.ts:deriveSAS exactly.
// Both encodeFields implementations use big-endian uint64 length prefixes per
// field; the base-36 encoding takes first 6 bytes MSB-first and iterates 12
// positions right-to-left, filling with '0' on leading zeros.
func DeriveSAS(genesisHash, headHash, newX25519Pub, newSignPub []byte) (string, error) {
	if len(genesisHash) == 0 || len(headHash) == 0 {
		return "", fmt.Errorf("seal: DeriveSAS: genesis_hash and head_hash must be non-empty")
	}
	digest := sha256.Sum256(encodeFields(
		[]byte(sasMagic),
		genesisHash,
		headHash,
		newX25519Pub,
		newSignPub,
	))
	// Extract first 6 bytes (48 bits) as a big-endian uint64 (top 16 bits unused).
	n := uint64(digest[0])<<40 | uint64(digest[1])<<32 | uint64(digest[2])<<24 |
		uint64(digest[3])<<16 | uint64(digest[4])<<8 | uint64(digest[5])
	const base = uint64(36)
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	code := make([]byte, 12)
	for i := 11; i >= 0; i-- {
		code[i] = chars[n%base]
		n /= base
	}
	return fmt.Sprintf("%s-%s-%s", code[0:4], code[4:8], code[8:12]), nil
}
