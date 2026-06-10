package journalkey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
)

func TestSecretIDConventions(t *testing.T) {
	id := SecretID("work")
	if id != "journal/work" {
		t.Fatalf("SecretID = %q, want journal/work", id)
	}
	if !IsJournalKey(id) {
		t.Fatal("IsJournalKey(journal/work) = false")
	}
	if IsJournalKey("creds/openai") {
		t.Fatal("IsJournalKey(creds/openai) = true; want false")
	}
	if MountName(id) != "work" {
		t.Fatalf("MountName = %q, want work", MountName(id))
	}
	if MountName("creds/openai") != "" {
		t.Fatal("MountName of non-journal id must be empty")
	}
}

func newOwnerDevice(t *testing.T) *seal.Device {
	t.Helper()
	m, err := seal.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	d, err := seal.DeviceFromMnemonic(m, "")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// newTargetNode mints a node sub-key holder with one fresh sub-key, returning
// the holder and its current SignedSubKey (the owner client seals to its pubkey).
func newTargetNode(t *testing.T, nodeID string, now time.Time) (*subkey.Node, subkey.SignedSubKey) {
	t.Helper()
	signKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	n := subkey.NewNode(signKey, nodeID, 72*time.Hour)
	if _, err := n.Rotate(now); err != nil {
		t.Fatal(err)
	}
	sk, ok := n.Current(now)
	if !ok {
		t.Fatal("node has no current sub-key after Rotate")
	}
	return n, sk
}

// TestJournalKeyRoundTrip is the full owner-sealed delivery round trip for a
// journal repo password: seal-to-owner -> (CP stores ciphertext) -> owner unseal
// + reseal-to-node -> node OpenDelivered -> original password. This is the
// owner-client-simulated path the cross-node resume uses.
func TestJournalKeyRoundTrip(t *testing.T) {
	now := time.Now()
	const password = "kopia-repo-pw-0123456789abcdef-the-secret"
	const spawnID = "spawn-rt"
	const ownerID = "owner-1"
	const gen = uint64(3)

	owner := newOwnerDevice(t)

	// (1) Origin node seals the repo password to the owner's device set.
	atRest := seal.AtRestAAD{AccountID: ownerID, SecretID: SecretID("work"), Version: 1}
	env, err := SealToOwner(password, []seal.X25519PubKey{owner.X25519PubKey()}, atRest)
	if err != nil {
		t.Fatalf("SealToOwner: %v", err)
	}

	// (2) The CP stores ONLY the ciphertext (round-trip through JSON to prove it
	// is opaque, serializable bytes — no key material needed to store it).
	stored, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var fetched seal.Envelope
	if err := json.Unmarshal(stored, &fetched); err != nil {
		t.Fatal(err)
	}

	// (3) Resume: the target node publishes a sub-key; the owner client re-seals
	// the fetched ciphertext to it under the in-flight delivery AAD.
	node, sk := newTargetNode(t, "node-B", now)
	inflight := seal.InFlightAAD{SpawnID: spawnID, Generation: gen, NodeID: "node-B", NotAfter: sk.NotAfter}
	sealedNode, err := ResealForNode(&fetched, owner.X25519Priv, sk.HPKEPub, inflight)
	if err != nil {
		t.Fatalf("ResealForNode: %v", err)
	}

	// (4) The node opens the delivery (NodeID + NotAfter supplied per retained
	// sub-key by OpenDelivered; the node knows only spawn + generation out-of-band).
	base := seal.InFlightAAD{SpawnID: spawnID, Generation: gen}
	pt, err := node.OpenDelivered(sealedNode, base, now)
	if err != nil {
		t.Fatalf("OpenDelivered: %v", err)
	}
	if string(pt) != password {
		t.Fatalf("recovered password = %q, want %q", pt, password)
	}
}

// TestJournalKeyDeliveryGenerationFencing verifies the delivery AAD binds the
// generation: a node opening with a DIFFERENT generation than the one the owner
// sealed for fails (a CP cannot replay a key into a stale/wrong episode).
func TestJournalKeyDeliveryGenerationFencing(t *testing.T) {
	now := time.Now()
	owner := newOwnerDevice(t)
	atRest := seal.AtRestAAD{AccountID: "o", SecretID: SecretID("work"), Version: 1}
	env, err := SealToOwner("pw", []seal.X25519PubKey{owner.X25519PubKey()}, atRest)
	if err != nil {
		t.Fatal(err)
	}
	node, sk := newTargetNode(t, "node-B", now)

	// Owner seals for generation 5.
	sealedNode, err := ResealForNode(env, owner.X25519Priv,
		sk.HPKEPub, seal.InFlightAAD{SpawnID: "s", Generation: 5, NodeID: "node-B", NotAfter: sk.NotAfter})
	if err != nil {
		t.Fatal(err)
	}

	// Node tries to open as generation 6 -> AAD mismatch, no match.
	if _, err := node.OpenDelivered(sealedNode, seal.InFlightAAD{SpawnID: "s", Generation: 6}, now); err == nil {
		t.Fatal("OpenDelivered with mismatched generation must fail")
	}
	// Correct generation still opens (sanity).
	if _, err := node.OpenDelivered(sealedNode, seal.InFlightAAD{SpawnID: "s", Generation: 5}, now); err != nil {
		t.Fatalf("OpenDelivered with correct generation: %v", err)
	}
}

// TestWrongOwnerDeviceCannotUnseal verifies a different owner device (not a
// recipient of the at-rest envelope) cannot recover the password to reseal it.
func TestWrongOwnerDeviceCannotUnseal(t *testing.T) {
	now := time.Now()
	owner := newOwnerDevice(t)
	stranger := newOwnerDevice(t)
	env, err := SealToOwner("pw", []seal.X25519PubKey{owner.X25519PubKey()},
		seal.AtRestAAD{AccountID: "o", SecretID: SecretID("work"), Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, sk := newTargetNode(t, "node-B", now)
	if _, err := ResealForNode(env, stranger.X25519Priv, sk.HPKEPub,
		seal.InFlightAAD{SpawnID: "s", Generation: 1, NodeID: "node-B", NotAfter: sk.NotAfter}); err == nil {
		t.Fatal("a non-recipient owner device must not be able to unseal+reseal the journal key")
	}
}
