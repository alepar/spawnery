package journal

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNotDelivered is returned by OwnerSealedCustody.PasswordFor before the
// owner has delivered the per-spawn repo password to this node. It is the
// "a different node cannot resume an owner-sealed spawn until the owner ceremony
// re-delivers the key" guard (design §4): a node holding only ErrNotDelivered
// cannot open the Kopia repo.
var ErrNotDelivered = errors.New("journal: owner-sealed repo password not yet delivered to this node")

// OwnerSealedCustody implements PasswordProvider for the owner-sealed durability
// class (design §4). Unlike NodeLocalCustody it does NOT generate or seal a
// password node-locally: the per-spawn Kopia repo password is custodied by the
// OWNER (sealed to the owner's device set, ciphertext-only at the CP) and
// DELIVERED to this node on every (cross-node) resume over the existing
// owner-sealed secret-delivery path (internal/cp DeliverSecrets -> node
// SecretDelivery -> OpenDelivered). The node's delivery handler calls Deliver to
// inject the unsealed password; PasswordFor then serves it to the journal
// Manager so the repo can be opened for Restore.
//
// A node that never received a delivery (a fresh target node before the owner
// ceremony completes, or a node that simply lacks the key) cannot open the repo:
// PasswordFor returns ErrNotDelivered.
//
// The §4 "upgrade node-local -> owner-sealed = seal the SAME password to the
// owner, no re-encryption" model means the ORIGIN node still mints + holds the
// password under NodeLocalCustody; OwnerSealedCustody is the RECEIVING side on a
// resume/migration target. The journal Manager routes a spawn to this custody
// once a key has been delivered for it (manager.go passwordFor).
type OwnerSealedCustody struct {
	mu      sync.Mutex
	m       map[string]deliveredKey
	waiters map[string][]chan struct{} // spawnID -> channels closed on first delivery
}

type deliveredKey struct {
	password string
	gen      uint64
}

// NewOwnerSealedCustody builds an empty owner-sealed custody. It holds no key
// material at rest — every key arrives by delivery and lives only in memory for
// the episode (the CP ciphertext is the durable copy).
func NewOwnerSealedCustody() *OwnerSealedCustody {
	return &OwnerSealedCustody{m: map[string]deliveredKey{}, waiters: map[string][]chan struct{}{}}
}

var _ PasswordProvider = (*OwnerSealedCustody)(nil)

// Deliver injects the unsealed repo password for spawnID at generation gen
// (the generation of the resume episode the key was sealed for). It is
// generation-fenced: a delivery whose gen is OLDER than one already accepted is
// rejected, so a stale/replayed episode key cannot clobber the live one. A
// same-or-newer gen supersedes (a fresh resume episode re-delivers). The first
// successful delivery for a spawn wakes any WaitDelivered waiters.
func (c *OwnerSealedCustody) Deliver(spawnID string, gen uint64, password string) error {
	if password == "" {
		return fmt.Errorf("journal: empty owner-sealed password for %s", spawnID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.m[spawnID]; ok && gen < cur.gen {
		return fmt.Errorf("journal: stale owner-sealed delivery for %s (gen %d < current %d)", spawnID, gen, cur.gen)
	}
	c.m[spawnID] = deliveredKey{password: password, gen: gen}
	// Wake waiters (idempotent: each channel is closed exactly once).
	for _, ch := range c.waiters[spawnID] {
		close(ch)
	}
	delete(c.waiters, spawnID)
	return nil
}

// PasswordFor implements PasswordProvider. It returns ErrNotDelivered until a
// password has been delivered for spawnID.
func (c *OwnerSealedCustody) PasswordFor(spawnID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.m[spawnID]
	if !ok {
		return "", ErrNotDelivered
	}
	return k.password, nil
}

// Delivered reports whether a password has been delivered for spawnID and, if
// so, the generation it was delivered at. The journal Manager uses this to route
// an owner-sealed spawn to this custody instead of the node-local default.
func (c *OwnerSealedCustody) Delivered(spawnID string) (gen uint64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, found := c.m[spawnID]
	return k.gen, found
}

// WaitDelivered blocks until a password has been delivered for spawnID or ctx is
// done. It is the "wait for the delivered key before journal.Restore" hook on
// the cross-node resume path (design §4/§5): the spawnlet bounds ctx with a
// timeout and falls back to the seeded dir (a defined non-hang state) if the
// owner ceremony does not complete in time. Returns nil once delivered.
func (c *OwnerSealedCustody) WaitDelivered(ctx context.Context, spawnID string) error {
	c.mu.Lock()
	if _, ok := c.m[spawnID]; ok {
		c.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	c.waiters[spawnID] = append(c.waiters[spawnID], ch)
	c.mu.Unlock()

	select {
	case <-ch:
		// Could be a real delivery or a Forget abandon; PasswordFor decides.
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Forget implements PasswordProvider: drop the in-memory delivered password
// (spawn delete / migrate-away). The owner-sealed ciphertext at the CP is the
// durable copy; this only clears the episode's plaintext from memory.
func (c *OwnerSealedCustody) Forget(spawnID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, spawnID)
	// Abandon any waiters: closing wakes them; PasswordFor will report
	// ErrNotDelivered. (Forget on a spawn nobody is resuming.)
	for _, ch := range c.waiters[spawnID] {
		close(ch)
	}
	delete(c.waiters, spawnID)
	return nil
}
