package spawnlet

import (
	"encoding/json"
	"fmt"

	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/storage/journal"
)

// SealedMount is a per-mount sealed journal key (result of SealJournalMounts).
type SealedMount struct {
	Mount      string
	Ciphertext []byte // JSON-marshaled seal.Envelope
}

// nodeLocalPassworder is the narrow interface that journal.Manager implements
// to expose the node-local password for owner-sealing (upgrade seam). It is
// kept unexported so only the spawnlet package uses this seam (avoid broad
// journal.JournalManager interface pollution).
type nodeLocalPassworder interface {
	NodeLocalPassword(spawnID string) (string, error)
}

// SealJournalMounts seals each journaled mount's node-local repo password to
// the owner's device set, returning JSON-marshaled seal.Envelopes per mount.
// Used by the UpgradeToOwnerSealed node handler (sp-8dkp §4 node side).
//
// ownerID is the owner's account id — bound into AtRestAAD so the ciphertext
// is identity-scoped and tamper-detectable. gen is the spawn generation (bound
// as AAD Version). ownerDevices are the raw X25519 pubkeys to seal to.
//
// If no journaler is installed or the journaler does not implement
// nodeLocalPassworder, returns an error so the node can report back to the CP.
func (m *Manager) SealJournalMounts(spawnID, ownerID string, gen uint64, ownerDevices []seal.X25519PubKey) ([]SealedMount, error) {
	if m.journal == nil {
		return nil, fmt.Errorf("seal journal mounts: no journaler installed on this node")
	}
	pwder, ok := m.journal.(nodeLocalPassworder)
	if !ok {
		return nil, fmt.Errorf("seal journal mounts: journaler does not expose node-local passwords")
	}

	sp, ok := m.store.Get(spawnID)
	if !ok {
		return nil, fmt.Errorf("seal journal mounts: spawn %q not found in store", spawnID)
	}

	// Collect the journaled (non-secret) mounts.
	var mounts []journal.Mount
	for _, jm := range sp.JournalMounts {
		if jm.Class.Journaled() && !jm.Secret {
			mounts = append(mounts, jm)
		}
	}
	if len(mounts) == 0 {
		return nil, fmt.Errorf("seal journal mounts: spawn %q has no journaled mounts to seal", spawnID)
	}

	out := make([]SealedMount, 0, len(mounts))
	for _, jm := range mounts {
		pw, err := pwder.NodeLocalPassword(spawnID)
		if err != nil {
			return nil, fmt.Errorf("seal journal mounts: get password for spawn %q mount %q: %w", spawnID, jm.Name, err)
		}
		aad := seal.AtRestAAD{
			AccountID: ownerID,
			SecretID:  journalkey.SecretID(jm.Name),
			Version:   gen,
		}
		env, err := journalkey.SealToOwner(pw, ownerDevices, aad)
		if err != nil {
			return nil, fmt.Errorf("seal journal mounts: seal mount %q: %w", jm.Name, err)
		}
		ct, err := json.Marshal(env)
		if err != nil {
			return nil, fmt.Errorf("seal journal mounts: marshal envelope for mount %q: %w", jm.Name, err)
		}
		out = append(out, SealedMount{Mount: jm.Name, Ciphertext: ct})
	}
	return out, nil
}
