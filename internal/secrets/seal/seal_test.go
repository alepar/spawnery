package seal

import (
	"bytes"
	"testing"
	"time"
)

// newTestDevice builds a Device from a fresh mnemonic.
func newTestDevice(t *testing.T) *Device {
	t.Helper()
	m, err := NewMnemonic()
	if err != nil {
		t.Fatalf("NewMnemonic: %v", err)
	}
	d, err := DeviceFromMnemonic(m, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic: %v", err)
	}
	return d
}

func TestSealOpenRoundTrip(t *testing.T) {
	d1 := newTestDevice(t)
	d2 := newTestDevice(t)
	payload := []byte("super-secret-api-key")
	aad := AtRestAAD{AccountID: "acct-1", SecretID: "sec-1", Version: 3}

	env, err := Seal(payload, []X25519PubKey{d1.X25519PubKey(), d2.X25519PubKey()}, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if len(env.Recipients) != 2 {
		t.Fatalf("want 2 recipients, got %d", len(env.Recipients))
	}

	for name, dev := range map[string]*Device{"device1": d1, "device2": d2} {
		got, err := Open(env, dev.X25519Priv)
		if err != nil {
			t.Fatalf("Open(%s): %v", name, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("Open(%s): got %q want %q", name, got, payload)
		}
	}
}

func TestOpenWrongDeviceRejected(t *testing.T) {
	recipient := newTestDevice(t)
	stranger := newTestDevice(t)
	env, err := Seal([]byte("x"), []X25519PubKey{recipient.X25519PubKey()}, AtRestAAD{AccountID: "a", SecretID: "s", Version: 1})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(env, stranger.X25519Priv); err == nil {
		t.Fatal("expected a non-recipient device to fail Open")
	}
}

func TestNoRecipients(t *testing.T) {
	if _, err := Seal([]byte("x"), nil, AtRestAAD{}); err != ErrNoRecipient {
		t.Fatalf("want ErrNoRecipient, got %v", err)
	}
}

// roast M2: every write mints a FRESH DEK — two Seals of the same payload to the
// same recipient must produce different DEKs AND different ciphertexts.
func TestFreshDEKPerWrite(t *testing.T) {
	d := newTestDevice(t)
	payload := []byte("same-payload")
	aad := AtRestAAD{AccountID: "a", SecretID: "s", Version: 1}

	env1, err := Seal(payload, []X25519PubKey{d.X25519PubKey()}, aad)
	if err != nil {
		t.Fatalf("Seal1: %v", err)
	}
	env2, err := Seal(payload, []X25519PubKey{d.X25519PubKey()}, aad)
	if err != nil {
		t.Fatalf("Seal2: %v", err)
	}

	// Externally: payload ciphertexts and recipient seals differ.
	if bytes.Equal(env1.CT, env2.CT) {
		t.Fatal("payload ciphertexts identical across writes (no fresh randomness)")
	}
	if bytes.Equal(env1.Recipients[0].CT, env2.Recipients[0].CT) {
		t.Fatal("recipient seals identical across writes")
	}

	// Internally (white-box): the actual DEKs differ.
	dek1, err := openDEK(env1, d.X25519Priv)
	if err != nil {
		t.Fatalf("openDEK1: %v", err)
	}
	dek2, err := openDEK(env2, d.X25519Priv)
	if err != nil {
		t.Fatalf("openDEK2: %v", err)
	}
	if bytes.Equal(dek1, dek2) {
		t.Fatal("roast M2 violated: two writes reused the same DEK")
	}
	// Both DEKs still recover the same plaintext.
	for _, env := range []*Envelope{env1, env2} {
		got, err := Open(env, d.X25519Priv)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("Open after fresh-DEK: got %q err %v", got, err)
		}
	}
}

// The CP must not be able to splice seals or replay an old version: rewriting
// any at-rest AAD field breaks authentication on Open.
func TestAtRestAADSpliceRejected(t *testing.T) {
	d := newTestDevice(t)
	base := AtRestAAD{AccountID: "acct-1", SecretID: "sec-1", Version: 5}
	env, err := Seal([]byte("v"), []X25519PubKey{d.X25519PubKey()}, base)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	tamper := []struct {
		name string
		mut  func(e *Envelope)
	}{
		{"version-downgrade", func(e *Envelope) { e.AtRest.Version = 4 }},
		{"account-splice", func(e *Envelope) { e.AtRest.AccountID = "acct-2" }},
		{"secret-splice", func(e *Envelope) { e.AtRest.SecretID = "sec-2" }},
	}
	for _, tc := range tamper {
		t.Run(tc.name, func(t *testing.T) {
			cp := *env
			cp.AtRest = env.AtRest
			tc.mut(&cp)
			if _, err := Open(&cp, d.X25519Priv); err == nil {
				t.Fatalf("expected tampered %s to fail Open", tc.name)
			}
		})
	}
}

func TestDeliveryRoundTrip(t *testing.T) {
	d := newTestDevice(t)
	nodePub, nodePriv, err := NodeKeyPair()
	if err != nil {
		t.Fatalf("NodeKeyPair: %v", err)
	}
	payload := []byte("the-gh-token")
	env, err := Seal(payload, []X25519PubKey{d.X25519PubKey()}, AtRestAAD{AccountID: "a", SecretID: "s", Version: 7})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	aad := InFlightAAD{
		SpawnID:    "spawn-1",
		Generation: 2,
		NodeID:     "node-9",
		NotAfter:   now.Add(time.Minute),
		Version:    7,
		DeliveryID: "del-abc",
	}
	sealed, err := ReSealToNode(env, d.X25519Priv, nodePub, aad)
	if err != nil {
		t.Fatalf("ReSealToNode: %v", err)
	}
	got, err := OpenFromOwner(sealed, nodePriv, aad, now)
	if err != nil {
		t.Fatalf("OpenFromOwner: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("delivery payload mismatch: got %q want %q", got, payload)
	}
}

// roast M11: the in-flight AAD rejection matrix — any mismatch in
// spawn/gen/node/version/deliveryId, or an expired notAfter, must reject.
func TestInFlightAADRejectionMatrix(t *testing.T) {
	d := newTestDevice(t)
	nodePub, nodePriv, err := NodeKeyPair()
	if err != nil {
		t.Fatalf("NodeKeyPair: %v", err)
	}
	env, err := Seal([]byte("p"), []X25519PubKey{d.X25519PubKey()}, AtRestAAD{AccountID: "a", SecretID: "s", Version: 1})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	sealAAD := InFlightAAD{
		SpawnID:    "spawn-1",
		Generation: 2,
		NodeID:     "node-9",
		NotAfter:   now.Add(time.Minute),
		Version:    1,
		DeliveryID: "del-1",
	}
	sealed, err := ReSealToNode(env, d.X25519Priv, nodePub, sealAAD)
	if err != nil {
		t.Fatalf("ReSealToNode: %v", err)
	}

	cases := []struct {
		name    string
		expect  InFlightAAD
		now     time.Time
		wantErr error
	}{
		{"wrong-spawn", with(sealAAD, func(a *InFlightAAD) { a.SpawnID = "spawn-X" }), now, ErrAADMismatch},
		{"wrong-generation", with(sealAAD, func(a *InFlightAAD) { a.Generation = 3 }), now, ErrAADMismatch},
		{"wrong-node", with(sealAAD, func(a *InFlightAAD) { a.NodeID = "node-X" }), now, ErrAADMismatch},
		{"wrong-version", with(sealAAD, func(a *InFlightAAD) { a.Version = 2 }), now, ErrAADMismatch},
		{"wrong-deliveryid", with(sealAAD, func(a *InFlightAAD) { a.DeliveryID = "del-2" }), now, ErrAADMismatch},
		// notAfter mismatch: if the node expects a different notAfter, AAD breaks.
		{"wrong-notafter-aad", with(sealAAD, func(a *InFlightAAD) { a.NotAfter = now.Add(2 * time.Minute) }), now, ErrAADMismatch},
		// notAfter clock check: even with the agreed AAD, an expired delivery is refused.
		{"expired-notafter", sealAAD, now.Add(2 * time.Minute), ErrExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := OpenFromOwner(sealed, nodePriv, tc.expect, tc.now)
			if err != tc.wantErr {
				t.Fatalf("got err %v, want %v", err, tc.wantErr)
			}
		})
	}

	// Sanity: the exact AAD at a valid time succeeds.
	if _, err := OpenFromOwner(sealed, nodePriv, sealAAD, now); err != nil {
		t.Fatalf("baseline OpenFromOwner failed: %v", err)
	}
}

// A ciphertext sealed to one node is useless to a different node key.
func TestDeliveryWrongNodeKeyRejected(t *testing.T) {
	d := newTestDevice(t)
	nodePub, _, _ := NodeKeyPair()
	_, otherPriv, _ := NodeKeyPair()
	env, _ := Seal([]byte("p"), []X25519PubKey{d.X25519PubKey()}, AtRestAAD{AccountID: "a", SecretID: "s", Version: 1})
	now := time.Unix(1_700_000_000, 0)
	aad := InFlightAAD{SpawnID: "s", Generation: 1, NodeID: "n", NotAfter: now.Add(time.Minute), Version: 1, DeliveryID: "d"}
	sealed, err := ReSealToNode(env, d.X25519Priv, nodePub, aad)
	if err != nil {
		t.Fatalf("ReSealToNode: %v", err)
	}
	if _, err := OpenFromOwner(sealed, otherPriv, aad, now); err == nil {
		t.Fatal("expected wrong node key to fail OpenFromOwner")
	}
}

func with(base InFlightAAD, mut func(*InFlightAAD)) InFlightAAD {
	cp := base
	mut(&cp)
	return cp
}
