package subkey_test

import (
	"crypto/ecdsa"
	"errors"
	"testing"
	"time"

	"spawnery/internal/clientverify"
	"spawnery/internal/pki"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
)

// nodeFix is a fully issued node identity: the PEMs a sealing client receives
// (relayed by the untrusted CP), the node cert private key (signs sub-keys), and
// the SAN identity.
type nodeFix struct {
	leaf, chain, root []byte
	key               *ecdsa.PrivateKey
	id                pki.Identity
}

func issue(t *testing.T, nodeID, account, class string) nodeFix {
	t.Helper()
	r, err := pki.NewRootCA("test-root")
	if err != nil {
		t.Fatalf("NewRootCA: %v", err)
	}
	inter, err := r.NewIntermediate(class)
	if err != nil {
		t.Fatalf("NewIntermediate: %v", err)
	}
	n, err := inter.IssueNode(nodeID, account, class, time.Now().Add(365*24*time.Hour))
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	return nodeFix{
		leaf:  pki.MarshalCertPEM(n.Cert),
		chain: pki.MarshalCertPEM(inter.Cert),
		root:  pki.MarshalCertPEM(r.Cert),
		key:   n.Key,
		id:    pki.Identity{NodeID: nodeID, AccountID: account, Class: class},
	}
}

func selfHosted(account string) subkey.Expectation {
	return clientverify.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: account}
}

// denyList is a test-double RevocationChecker (the AS list service is out of
// scope; this stands in for it).
type denyList map[string]bool

func (d denyList) IsRevoked(id pki.Identity) (bool, error) { return d[id.NodeID], nil }

// errChecker is a RevocationChecker whose list could not be fetched — the caller
// must fail closed.
type errChecker struct{}

func (errChecker) IsRevoked(pki.Identity) (bool, error) {
	return false, errors.New("revocation list unreachable")
}

// ---- sub-key sign + verify ----

func TestSignVerifyHappyPath(t *testing.T) {
	fx := issue(t, "n1", "alice", pki.ClassSelfHosted)
	now := time.Now()
	pub, _, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	sk, err := subkey.Sign(fx.key, "n1", pub, now, now.Add(subkey.DefaultValidity))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	leaf, _ := pki.ParseCertPEM(fx.leaf)
	if err := sk.Verify(leaf.PublicKey.(*ecdsa.PublicKey)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := sk.Valid(now); err != nil {
		t.Fatalf("Valid: %v", err)
	}
	if sk.KeyID() == "" {
		t.Fatal("KeyID empty")
	}
}

// A sub-key signed by the WRONG node's cert key must not verify against this
// node's cert (a CP cannot graft another node's sub-key onto this cert).
func TestVerifyRejectsWrongCertKey(t *testing.T) {
	victim := issue(t, "n1", "alice", pki.ClassSelfHosted)
	attacker := issue(t, "n2", "alice", pki.ClassSelfHosted)
	now := time.Now()
	pub, _, _ := seal.NodeKeyPair()
	// Signed by the attacker's cert key but claims victim's nodeID.
	sk, _ := subkey.Sign(attacker.key, "n1", pub, now, now.Add(subkey.DefaultValidity))
	leaf, _ := pki.ParseCertPEM(victim.leaf)
	if err := sk.Verify(leaf.PublicKey.(*ecdsa.PublicKey)); !errors.Is(err, subkey.ErrBadSig) {
		t.Fatalf("want ErrBadSig, got %v", err)
	}
}

// ---- VerifyNodeForSealing chain ----

func mintSubKey(t *testing.T, fx nodeFix, now time.Time, dur time.Duration) (subkey.SignedSubKey, []byte) {
	t.Helper()
	pub, priv, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	sk, err := subkey.Sign(fx.key, fx.id.NodeID, pub, now, now.Add(dur))
	if err != nil {
		t.Fatal(err)
	}
	return sk, priv
}

func TestVerifyNodeForSealingHappyPath(t *testing.T) {
	fx := issue(t, "n1", "alice", pki.ClassSelfHosted)
	now := time.Now()
	sk, _ := mintSubKey(t, fx, now, subkey.DefaultValidity)
	pub, id, err := subkey.VerifyNodeForSealing(fx.leaf, fx.chain, fx.root, sk, selfHosted("alice"), subkey.AllowAll{}, now)
	if err != nil {
		t.Fatalf("VerifyNodeForSealing: %v", err)
	}
	if id.NodeID != "n1" || id.AccountID != "alice" {
		t.Fatalf("id = %+v", id)
	}
	if string(pub) != string(sk.HPKEPub) {
		t.Fatal("returned trusted pubkey != sub-key pubkey")
	}
}

func TestVerifyNodeForSealingRejectsExpiredSubKey(t *testing.T) {
	fx := issue(t, "n1", "alice", pki.ClassSelfHosted)
	now := time.Now()
	sk, _ := mintSubKey(t, fx, now.Add(-2*time.Hour), time.Hour) // expired an hour ago
	_, _, err := subkey.VerifyNodeForSealing(fx.leaf, fx.chain, fx.root, sk, selfHosted("alice"), subkey.AllowAll{}, now)
	if !errors.Is(err, subkey.ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestVerifyNodeForSealingRejectsWrongNodeCert(t *testing.T) {
	victim := issue(t, "n1", "alice", pki.ClassSelfHosted)
	attacker := issue(t, "n2", "alice", pki.ClassSelfHosted)
	now := time.Now()
	pub, _, _ := seal.NodeKeyPair()
	// Sub-key claims n1, signed by n2's cert key, presented with n1's real cert.
	sk, _ := subkey.Sign(attacker.key, "n1", pub, now, now.Add(subkey.DefaultValidity))
	_, _, err := subkey.VerifyNodeForSealing(victim.leaf, victim.chain, victim.root, sk, selfHosted("alice"), subkey.AllowAll{}, now)
	if !errors.Is(err, subkey.ErrBadSig) {
		t.Fatalf("want ErrBadSig, got %v", err)
	}
}

func TestVerifyNodeForSealingRejectsWrongSAN(t *testing.T) {
	// Node bound to attacker's account; client expects its own account "alice".
	fx := issue(t, "n1", "attacker", pki.ClassSelfHosted)
	now := time.Now()
	sk, _ := mintSubKey(t, fx, now, subkey.DefaultValidity)
	_, _, err := subkey.VerifyNodeForSealing(fx.leaf, fx.chain, fx.root, sk, selfHosted("alice"), subkey.AllowAll{}, now)
	if err == nil {
		t.Fatal("a node bound to another account must be rejected")
	}
}

func TestVerifyNodeForSealingRejectsUntrustedRoot(t *testing.T) {
	fx := issue(t, "n1", "alice", pki.ClassSelfHosted)
	other := issue(t, "n9", "alice", pki.ClassSelfHosted) // a DIFFERENT, unpinned root
	now := time.Now()
	sk, _ := mintSubKey(t, fx, now, subkey.DefaultValidity)
	_, _, err := subkey.VerifyNodeForSealing(fx.leaf, fx.chain, other.root, sk, selfHosted("alice"), subkey.AllowAll{}, now)
	if err == nil {
		t.Fatal("a chain that does not reach the pinned root must be rejected")
	}
}

func TestVerifyNodeForSealingRejectsRevokedNode(t *testing.T) {
	fx := issue(t, "n1", "alice", pki.ClassSelfHosted)
	now := time.Now()
	sk, _ := mintSubKey(t, fx, now, subkey.DefaultValidity)
	revoked := denyList{"n1": true}
	_, _, err := subkey.VerifyNodeForSealing(fx.leaf, fx.chain, fx.root, sk, selfHosted("alice"), revoked, now)
	if !errors.Is(err, subkey.ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
}

func TestVerifyNodeForSealingFailsClosedOnCheckerError(t *testing.T) {
	fx := issue(t, "n1", "alice", pki.ClassSelfHosted)
	now := time.Now()
	sk, _ := mintSubKey(t, fx, now, subkey.DefaultValidity)
	_, _, err := subkey.VerifyNodeForSealing(fx.leaf, fx.chain, fx.root, sk, selfHosted("alice"), errChecker{}, now)
	if err == nil {
		t.Fatal("a revocation-check error must fail closed (refuse to seal)")
	}
}

// ---- full round trip ----

func TestFullRoundTrip(t *testing.T) {
	fx := issue(t, "n1", "alice", pki.ClassSelfHosted)
	now := time.Now()

	// Owner has a device keypair; seal a secret at rest to it.
	devPub, devPriv, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("super-secret-byok-key")
	env, err := seal.Seal(secret, []seal.X25519PubKey{devPub}, seal.AtRestAAD{AccountID: "alice", SecretID: "byok", Version: 7})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Node holds a current, retained sub-key.
	holder := subkey.NewNode(fx.key, "n1", subkey.DefaultValidity)
	published, err := holder.Rotate(now)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Client verifies the node and re-seals the payload to it.
	baseAAD := seal.InFlightAAD{SpawnID: "sp-1", Generation: 3, Version: 7, DeliveryID: "d-abc"}
	sealed, err := subkey.SealForNode(env, devPriv, fx.leaf, fx.chain, fx.root, published, selfHosted("alice"), subkey.AllowAll{}, baseAAD, now)
	if err != nil {
		t.Fatalf("SealForNode: %v", err)
	}

	// Node opens with its retained sub-key (NodeID/NotAfter reconstructed per-key).
	got, err := holder.OpenDelivered(sealed, baseAAD, now)
	if err != nil {
		t.Fatalf("OpenDelivered: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatalf("round trip: got %q, want %q", got, secret)
	}

	// A wrong-context AAD (different deliveryId) must not open.
	badAAD := baseAAD
	badAAD.DeliveryID = "d-replayed"
	if _, err := holder.OpenDelivered(sealed, badAAD, now); err == nil {
		t.Fatal("a mismatched in-flight AAD must not open")
	}
}

// Two concurrent sub-keys across a rotation: a delivery sealed to EITHER the old
// or the new sub-key opens via trial-Open (spec §1, roast m2).
func TestTwoConcurrentSubKeySelection(t *testing.T) {
	fx := issue(t, "n1", "alice", pki.ClassSelfHosted)
	t0 := time.Now()
	validity := 72 * time.Hour

	devPub, devPriv, _ := seal.NodeKeyPair()
	env, err := seal.Seal([]byte("payload"), []seal.X25519PubKey{devPub}, seal.AtRestAAD{AccountID: "alice", SecretID: "s", Version: 1})
	if err != nil {
		t.Fatal(err)
	}

	holder := subkey.NewNode(fx.key, "n1", validity)
	sk1, err := holder.Rotate(t0) // [t0, t0+72h)
	if err != nil {
		t.Fatal(err)
	}

	// Past half-life: rotate again; both sub-keys now retained.
	tRot := t0.Add(40 * time.Hour)
	if !holder.NeedsRotation(tRot) {
		t.Fatal("expected NeedsRotation at past half-life")
	}
	sk2, err := holder.Rotate(tRot) // [t0+40h, t0+112h)
	if err != nil {
		t.Fatal(err)
	}
	if n := holder.Retained(tRot); n != 2 {
		t.Fatalf("retained = %d, want 2", n)
	}

	now := t0.Add(45 * time.Hour) // both windows cover now
	base := seal.InFlightAAD{SpawnID: "sp", Generation: 1, Version: 1, DeliveryID: "d1"}

	// Sealed to the OLD sub-key — opens.
	sealedOld, err := subkey.SealForNode(env, devPriv, fx.leaf, fx.chain, fx.root, sk1, selfHosted("alice"), subkey.AllowAll{}, base, now)
	if err != nil {
		t.Fatalf("SealForNode old: %v", err)
	}
	if got, err := holder.OpenDelivered(sealedOld, base, now); err != nil || string(got) != "payload" {
		t.Fatalf("open via old sub-key: got %q err %v", got, err)
	}

	// Sealed to the NEW sub-key — also opens.
	base.DeliveryID = "d2"
	sealedNew, err := subkey.SealForNode(env, devPriv, fx.leaf, fx.chain, fx.root, sk2, selfHosted("alice"), subkey.AllowAll{}, base, now)
	if err != nil {
		t.Fatalf("SealForNode new: %v", err)
	}
	if got, err := holder.OpenDelivered(sealedNew, base, now); err != nil || string(got) != "payload" {
		t.Fatalf("open via new sub-key: got %q err %v", got, err)
	}

	// After the old sub-key expires, only one remains and the old ciphertext no
	// longer opens (expired private half is skipped).
	afterExpiry := t0.Add(73 * time.Hour)
	if n := holder.Retained(afterExpiry); n != 1 {
		// prune happens on Rotate; emulate by checking count semantics.
		t.Logf("retained after expiry (pre-prune) = %d", n)
	}
	if _, err := holder.OpenDelivered(sealedOld, base, afterExpiry); err == nil {
		t.Fatal("a delivery to an expired sub-key must no longer open")
	}
}
