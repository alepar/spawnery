package node

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
)

// Owner-sealed secret delivery, node side (sp-2ckv.4, design §3/§6). The node holds the HPKE sub-key
// private halves (cfg.SubKeys, a subkey.Node); the CP relays owner-sealed ciphertext it cannot read.
// On SecretDelivery the node trial-Opens each ciphertext across its retained sub-keys (enforcing AAD
// equality + the notAfter clock check), then writes the plaintext into the spawn's tmpfs secrets dir
// at the declared path (0600). Plaintext exists only in this process's memory and that tmpfs.

type secretDeliveryReplayKey struct {
	spawnID    string
	generation uint64
	secretID   string
}

type secretDeliveryReplayState struct {
	highWater uint64
	seen      map[string]struct{}
	busy      bool
	pruned    bool
	cond      *sync.Cond
}

type secretDeliveryReplay struct {
	mu         sync.Mutex
	deliveries map[secretDeliveryReplayKey]*secretDeliveryReplayState
}

func newSecretDeliveryReplay() *secretDeliveryReplay {
	return &secretDeliveryReplay{deliveries: map[secretDeliveryReplayKey]*secretDeliveryReplayState{}}
}

func (r *secretDeliveryReplay) begin(spawnID string, generation uint64, secretID string, version uint64, deliveryID string) (func(), func(), error) {
	if version == 0 {
		return nil, nil, fmt.Errorf("missing delivery version")
	}
	if deliveryID == "" {
		return nil, nil, fmt.Errorf("missing delivery id")
	}

	key := secretDeliveryReplayKey{spawnID: spawnID, generation: generation, secretID: secretID}
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.deliveries[key]
	if st == nil {
		st = &secretDeliveryReplayState{seen: map[string]struct{}{}}
		st.cond = sync.NewCond(&r.mu)
		r.deliveries[key] = st
	}
	for st.busy && !st.pruned {
		st.cond.Wait()
	}
	if st.pruned {
		return nil, nil, fmt.Errorf("delivery generation pruned for spawn=%s generation=%d secret=%s", spawnID, generation, secretID)
	}
	if version < st.highWater {
		return nil, nil, fmt.Errorf("stale delivery version %d below accepted high-water %d", version, st.highWater)
	}
	if _, ok := st.seen[deliveryID]; ok {
		return nil, nil, fmt.Errorf("duplicate delivery id %q", deliveryID)
	}
	st.seen[deliveryID] = struct{}{}
	st.busy = true

	done := false
	commit := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if done {
			return
		}
		done = true
		if version > st.highWater {
			st.highWater = version
		}
		st.busy = false
		st.cond.Broadcast()
		if st.pruned {
			delete(r.deliveries, key)
		}
	}
	rollback := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if done {
			return
		}
		done = true
		delete(st.seen, deliveryID)
		st.busy = false
		st.cond.Broadcast()
		if st.pruned {
			delete(r.deliveries, key)
		}
	}
	return commit, rollback, nil
}

func (r *secretDeliveryReplay) checkBeforeConsume(spawnID string, generation uint64, secretID string, version uint64) error {
	key := secretDeliveryReplayKey{spawnID: spawnID, generation: generation, secretID: secretID}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.deliveries[key]
	if st == nil {
		return fmt.Errorf("delivery replay lease missing")
	}
	if st.pruned {
		return fmt.Errorf("delivery generation pruned for spawn=%s generation=%d secret=%s", spawnID, generation, secretID)
	}
	if version < st.highWater {
		return fmt.Errorf("stale delivery version %d below accepted high-water %d", version, st.highWater)
	}
	return nil
}

func (r *secretDeliveryReplay) pruneSpawnOlderThan(spawnID string, generation uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, st := range r.deliveries {
		if key.spawnID == spawnID && key.generation < generation {
			if st.busy {
				st.pruned = true
				st.cond.Broadcast()
				continue
			}
			delete(r.deliveries, key)
		}
	}
}

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
	if a.secretReplay == nil {
		log.Printf("secret-delivery for %s dropped: node has no replay guard", sd.SpawnId)
		return
	}
	a.secretReplay.pruneSpawnOlderThan(sd.SpawnId, sd.Generation)
	now := time.Now()
	for _, sec := range sd.Secrets {
		var sealed seal.NodeSealed
		if err := json.Unmarshal(sec.Sealed, &sealed); err != nil {
			log.Printf("secret-delivery %s/%s: malformed sealed payload: %v", sd.SpawnId, sec.SecretId, err)
			continue
		}
		base := seal.InFlightAAD{
			SpawnID:    sd.SpawnId,
			Generation: sd.Generation,
			Version:    sec.Version,
			DeliveryID: sec.DeliveryId,
		}
		commit, rollback, err := a.secretReplay.begin(sd.SpawnId, sd.Generation, sec.SecretId, sec.Version, sec.DeliveryId)
		if err != nil {
			log.Printf("secret-delivery %s/%s: replay rejected: %v", sd.SpawnId, sec.SecretId, err)
			continue
		}
		a.subkeysMu.Lock()
		pt, err := a.cfg.SubKeys.OpenDelivered(&sealed, base, now)
		a.subkeysMu.Unlock()
		if err != nil {
			rollback()
			log.Printf("secret-delivery %s/%s: unseal failed: %v", sd.SpawnId, sec.SecretId, err)
			continue
		}
		if err := a.secretReplay.checkBeforeConsume(sd.SpawnId, sd.Generation, sec.SecretId, sec.Version); err != nil {
			for i := range pt {
				pt[i] = 0
			}
			rollback()
			log.Printf("secret-delivery %s/%s: replay rejected before consume: %v", sd.SpawnId, sec.SecretId, err)
			continue
		}

		// Journal-key deliveries (transient-tier §4, sp-u53.5.4) carry the per-spawn
		// Kopia repo password, NOT a tmpfs secret: route the plaintext into the
		// journaler's owner-sealed custody so a cross-node resume can open the repo
		// before journal.Restore. secret_id namespaces these (journalkey.Prefix).
		if journalkey.IsJournalKey(sec.SecretId) {
			derr := a.mgr.DeliverJournalKey(sd.SpawnId, sd.Generation, string(pt))
			for i := range pt {
				pt[i] = 0
			}
			if derr != nil {
				rollback()
				log.Printf("secret-delivery %s/%s: journal key inject failed: %v", sd.SpawnId, sec.SecretId, derr)
				continue
			}
			commit()
			log.Printf("secret-delivery %s: injected journal key %q (gen %d)", sd.SpawnId, sec.SecretId, sd.Generation)
			continue
		}

		path, werr := a.mgr.InjectSecret(sd.SpawnId, sec.TargetPath, pt)
		// Zero the plaintext copy we hold once written (defense-in-depth, §6 — not a hard guarantee under
		// Go's GC, but cheap and removes the obvious lingering buffer).
		for i := range pt {
			pt[i] = 0
		}
		if werr != nil {
			rollback()
			log.Printf("secret-delivery %s/%s: write %q: %v", sd.SpawnId, sec.SecretId, sec.TargetPath, werr)
			continue
		}
		commit()
		log.Printf("secret-delivery %s: injected secret %q -> %s", sd.SpawnId, sec.SecretId, path)
	}
}
