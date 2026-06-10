package node

import (
	"encoding/json"
	"log"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/secrets/seal"
)

// Owner-sealed secret delivery, node side (sp-2ckv.4, design §3/§6). The node holds the HPKE sub-key
// private halves (cfg.SubKeys, a subkey.Node); the CP relays owner-sealed ciphertext it cannot read.
// On SecretDelivery the node trial-Opens each ciphertext across its retained sub-keys (enforcing AAD
// equality + the notAfter clock check), then writes the plaintext into the spawn's tmpfs secrets dir
// at the declared path (0600). Plaintext exists only in this process's memory and that tmpfs.

// publishSubKey returns the current SignedSubKey JSON to advertise on Register, rotating it first if it
// is past half-life (or absent). Returns nil when the node has no sub-key holder (insecure/dev mode) —
// then no sub-key is published and SecretDelivery is rejected. Concurrency-safe (subkeysMu).
func (a *attacher) publishSubKey(now time.Time) []byte {
	a.subkeysMu.Lock()
	defer a.subkeysMu.Unlock()
	return a.currentSubKeyBytesLocked(now)
}

// rotatedSubKey is publishSubKey for the heartbeat path: it rotates if needed but returns the bytes
// only when the published sub-key actually CHANGED since the last publish (so a steady-state heartbeat
// carries no sub-key). Concurrency-safe (subkeysMu).
func (a *attacher) rotatedSubKey(now time.Time) []byte {
	a.subkeysMu.Lock()
	defer a.subkeysMu.Unlock()
	b := a.currentSubKeyBytesLocked(now)
	if b == nil {
		return nil
	}
	id := a.lastSubKeyID
	cur, ok := a.cfg.SubKeys.Current(now)
	if !ok {
		return nil
	}
	if cur.KeyID() == id {
		return nil // unchanged since last publish — don't re-send
	}
	return b
}

// currentSubKeyBytesLocked rotates-at-half-life and returns the current SignedSubKey JSON, recording
// its KeyID as the last published one. Caller holds subkeysMu.
func (a *attacher) currentSubKeyBytesLocked(now time.Time) []byte {
	if a.cfg.SubKeys == nil {
		return nil
	}
	if a.cfg.SubKeys.NeedsRotation(now) {
		if _, err := a.cfg.SubKeys.Rotate(now); err != nil {
			log.Printf("subkey: rotate: %v", err)
			return nil
		}
	}
	cur, ok := a.cfg.SubKeys.Current(now)
	if !ok {
		return nil
	}
	b, err := json.Marshal(cur)
	if err != nil {
		log.Printf("subkey: marshal current: %v", err)
		return nil
	}
	a.lastSubKeyID = cur.KeyID()
	return b
}

// handleSecretDelivery unseals each owner-sealed secret in a SecretDelivery and writes the plaintext to
// the spawn's tmpfs secrets dir at its target path. Generation fencing is done by the caller (handle).
// Runs on its own goroutine. A node with no sub-key holder (insecure/dev) drops the delivery.
func (a *attacher) handleSecretDelivery(sd *nodev1.SecretDelivery) {
	if a.cfg.SubKeys == nil {
		log.Printf("secret-delivery for %s dropped: node has no HPKE sub-key holder", sd.SpawnId)
		return
	}
	now := time.Now()
	// The in-flight AAD context the node knows out-of-band (design §3 M11). NodeID + NotAfter are filled
	// per-retained-sub-key by OpenDelivered. Version + DeliveryID are 0/"" for this slice: the
	// version-monotonic and deliveryId-once stateful checks are documented follow-up hooks (sp-u53.5.4),
	// matching seal.OpenFromOwner's contract — the owner seals with the same zero values, so AAD matches.
	base := seal.InFlightAAD{
		SpawnID:    sd.SpawnId,
		Generation: sd.Generation,
	}
	for _, sec := range sd.Secrets {
		var sealed seal.NodeSealed
		if err := json.Unmarshal(sec.Sealed, &sealed); err != nil {
			log.Printf("secret-delivery %s/%s: malformed sealed payload: %v", sd.SpawnId, sec.SecretId, err)
			continue
		}
		a.subkeysMu.Lock()
		pt, err := a.cfg.SubKeys.OpenDelivered(&sealed, base, now)
		a.subkeysMu.Unlock()
		if err != nil {
			log.Printf("secret-delivery %s/%s: unseal failed: %v", sd.SpawnId, sec.SecretId, err)
			continue
		}
		path, werr := a.mgr.InjectSecret(sd.SpawnId, sec.TargetPath, pt)
		// Zero the plaintext copy we hold once written (defense-in-depth, §6 — not a hard guarantee under
		// Go's GC, but cheap and removes the obvious lingering buffer).
		for i := range pt {
			pt[i] = 0
		}
		if werr != nil {
			log.Printf("secret-delivery %s/%s: write %q: %v", sd.SpawnId, sec.SecretId, sec.TargetPath, werr)
			continue
		}
		log.Printf("secret-delivery %s: injected secret %q -> %s", sd.SpawnId, sec.SecretId, path)
	}
}
