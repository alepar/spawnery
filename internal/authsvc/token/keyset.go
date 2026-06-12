package token

import (
	"crypto/ed25519"
)

// Key is one pinned AS session pubkey, addressed by its derived key_id.
type Key struct {
	KeyID string
	Pub   ed25519.PublicKey
}

// KeySet is the small ORDERED set of AS session pubkeys a verifier pins (current first, then
// next) [AM4]. Rotation: pre-publish next key to configs -> overlap window (both valid) -> AS
// switches signing -> retire old. Retiring a key from the set refuses every token it signed.
type KeySet []Key

// NewKeySet derives key_ids for the given public keys, preserving order and dropping
// duplicates.
func NewKeySet(pubs ...ed25519.PublicKey) (KeySet, error) {
	var ks KeySet
	for _, pub := range pubs {
		id, err := KeyID(pub)
		if err != nil {
			return nil, err
		}
		dup := false
		for _, k := range ks {
			if k.KeyID == id && equalPub(k.Pub, pub) {
				dup = true
				break
			}
		}
		if !dup {
			ks = append(ks, Key{KeyID: id, Pub: pub})
		}
	}
	return ks, nil
}

// Lookup returns the pubkey for a key_id, or false if the id is unknown (retired or forged).
func (ks KeySet) Lookup(keyID string) (ed25519.PublicKey, bool) {
	for _, k := range ks {
		if k.KeyID == keyID {
			return k.Pub, true
		}
	}
	return nil, false
}
