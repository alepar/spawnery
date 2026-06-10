// Package journalkey composes the owner-sealed-secrets primitives
// (internal/secrets/seal + internal/secrets/subkey) for ONE specific payload:
// the per-spawn Kopia journal repo password (transient-tier design §4). It does
// NOT reinvent crypto — it is a thin convention layer that:
//
//   - defines the journal-key secret_id namespace (Prefix) so the node's
//     SecretDelivery handler can recognise a delivered journal password and route
//     it into journal.OwnerSealedCustody instead of the secrets tmpfs;
//   - seals a node-held repo password to the OWNER's device set (SealToOwner),
//     producing the ciphertext the CP stores (ciphertext-only — design §4);
//   - re-seals that owner ciphertext to a target node's HPKE sub-key
//     (ResealForNode), the owner-client leg of a (cross-node) resume, delivered
//     over the existing DeliverSecrets/SecretDelivery path.
//
// The matching node-side open is subkey.Node.OpenDelivered (unchanged); the
// matching custody inject is journal.OwnerSealedCustody.Deliver. This package is
// pure crypto/string plumbing — no I/O, no journal/cp imports — so it stays
// hermetically testable and free of import cycles.
//
// Design: docs/superpowers/specs/2026-06-10-transient-tier-kopia-journal-design.md §4
// couples with docs/superpowers/specs/2026-06-10-owner-sealed-secrets-design.md §3.
package journalkey

import (
	"strings"

	"spawnery/internal/secrets/seal"
)

// Prefix namespaces journal-key deliveries inside the shared SealedSecret
// secret_id space. A SecretDelivery whose secret_id begins with Prefix carries a
// Kopia repo password destined for journal.OwnerSealedCustody (not the secrets
// tmpfs). The delivered plaintext is the per-spawn repo password; the suffix is
// the originating mount name (a label — the password is per-spawn-repo, design
// §1b repo-per-spawn).
const Prefix = "journal/"

// SecretID returns the journal-key secret_id for a mount (e.g. "journal/work").
func SecretID(mount string) string { return Prefix + mount }

// IsJournalKey reports whether a SecretDelivery secret_id carries a journal repo
// password (vs an ordinary tmpfs secret).
func IsJournalKey(secretID string) bool { return strings.HasPrefix(secretID, Prefix) }

// MountName extracts the originating mount label from a journal-key secret_id.
// Returns "" when secretID is not a journal key.
func MountName(secretID string) string {
	if !IsJournalKey(secretID) {
		return ""
	}
	return strings.TrimPrefix(secretID, Prefix)
}

// SealToOwner seals a node-held repo password to the owner's enrolled device set,
// producing the at-rest Envelope the CP stores as ciphertext only (design §4
// "the CP stores ONLY ciphertext"). It is the node-side leg of node-local ->
// owner-sealed upgrade: the SAME password is additionally sealed to the owner
// (no repo re-encryption). aad binds (owner/account id, secret id, version) so a
// compromised CP cannot splice or version-downgrade the stored ciphertext.
//
// ownerDevices are the X25519 (HPKE) pubkeys of the owner's currently enrolled
// devices — obtained from the owner device registry (the registry FETCH is a CP
// seam not yet wired; see internal/cp/journalkeys.OwnerDeviceRegistry). Sealing
// needs only the public halves, so the node can perform it.
func SealToOwner(password string, ownerDevices []seal.X25519PubKey, aad seal.AtRestAAD) (*seal.Envelope, error) {
	return seal.Seal([]byte(password), ownerDevices, aad)
}

// ResealForNode is the owner-client leg of a (cross-node) resume: the owner
// unseals the stored Envelope with one of its device private keys and re-seals
// the recovered password to the target node's verified HPKE sub-key under the
// in-flight delivery AAD (design §3). The resulting NodeSealed is what the owner
// client hands to DeliverSecrets (secret_id = SecretID(mount)); the target node
// opens it with subkey.Node.OpenDelivered and feeds the plaintext into
// journal.OwnerSealedCustody.Deliver.
//
// nodeHPKEPub MUST be the pubkey from a sub-key the client has already verified
// against the node cert chain + pinned roots (subkey.VerifyNodeForSealing); this
// function takes it as trusted input (the seal package's PKI boundary).
func ResealForNode(env *seal.Envelope, deviceX25519Priv []byte, nodeHPKEPub []byte, aad seal.InFlightAAD) (*seal.NodeSealed, error) {
	return seal.ReSealToNode(env, deviceX25519Priv, nodeHPKEPub, aad)
}
