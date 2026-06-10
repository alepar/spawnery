package seal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	bip39 "github.com/tyler-smith/go-bip39"
)

// BIP-39 determinism: the same mnemonic always derives the same X25519 + P-256
// keypairs (the basis of recovery without escrow); a different mnemonic differs.
func TestBIP39Determinism(t *testing.T) {
	m, err := NewMnemonic()
	if err != nil {
		t.Fatalf("NewMnemonic: %v", err)
	}
	a, err := DeviceFromMnemonic(m, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic a: %v", err)
	}
	b, err := DeviceFromMnemonic(m, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic b: %v", err)
	}
	if !bytes.Equal(a.X25519Priv, b.X25519Priv) || !bytes.Equal(a.X25519Pub, b.X25519Pub) {
		t.Fatal("X25519 derivation not deterministic")
	}
	if a.Sign.D.Cmp(b.Sign.D) != 0 {
		t.Fatal("P-256 derivation not deterministic")
	}
	if !bytes.Equal(marshalSignPub(a.SignPub()), marshalSignPub(b.SignPub())) {
		t.Fatal("P-256 pubkey not deterministic")
	}

	// A passphrase changes the seed → different keys.
	c, err := DeviceFromMnemonic(m, "extra")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic c: %v", err)
	}
	if bytes.Equal(a.X25519Priv, c.X25519Priv) {
		t.Fatal("passphrase did not change derivation")
	}

	// A different mnemonic → different keys.
	m2, _ := NewMnemonic()
	d2, _ := DeviceFromMnemonic(m2, "")
	if bytes.Equal(a.X25519Priv, d2.X25519Priv) {
		t.Fatal("distinct mnemonics produced the same key")
	}
}

func TestInvalidMnemonic(t *testing.T) {
	if _, err := DeviceFromMnemonic("not a valid bip39 phrase at all", ""); err == nil {
		t.Fatal("expected invalid mnemonic to error")
	}
}

// The X25519 and P-256 sub-keys are domain-separated: they are not the same
// bytes and neither trivially reveals the other.
func TestSubKeysDomainSeparated(t *testing.T) {
	d := newTestDevice(t)
	if bytes.Equal(d.X25519Priv, d.Sign.D.Bytes()) {
		t.Fatal("X25519 and P-256 private scalars are identical (not domain-separated)")
	}
}

// recovery virtual device round-trips a seal sealed to its pubkey.
func TestRecoveryVirtualDeviceUnseal(t *testing.T) {
	recCode, err := NewMnemonic()
	if err != nil {
		t.Fatalf("NewMnemonic: %v", err)
	}
	rec, err := RecoveryDevice(recCode)
	if err != nil {
		t.Fatalf("RecoveryDevice: %v", err)
	}
	device := newTestDevice(t)

	payload := []byte("recover-me")
	// Seal to both the device and the recovery virtual device (the always-
	// enrolled recipient).
	env, err := Seal(payload, []X25519PubKey{device.X25519PubKey(), rec.X25519PubKey()}, AtRestAAD{AccountID: "a", SecretID: "s", Version: 1})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Reconstruct the recovery device from just the code and unseal.
	rec2, err := RecoveryDevice(recCode)
	if err != nil {
		t.Fatalf("RecoveryDevice reconstruct: %v", err)
	}
	got, err := Open(env, rec2.X25519Priv)
	if err != nil {
		t.Fatalf("recovery Open: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("recovery payload mismatch: got %q", got)
	}
}

func TestKeyfileRoundTrip(t *testing.T) {
	d := newTestDevice(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "device.json")
	if err := d.WriteKeyfile(path); err != nil {
		t.Fatalf("WriteKeyfile: %v", err)
	}

	// Must be 0600.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keyfile perm = %o, want 0600", perm)
	}

	loaded, err := LoadKeyfile(path)
	if err != nil {
		t.Fatalf("LoadKeyfile: %v", err)
	}
	if !bytes.Equal(loaded.X25519Priv, d.X25519Priv) || !bytes.Equal(loaded.X25519Pub, d.X25519Pub) {
		t.Fatal("X25519 material did not round-trip")
	}
	if loaded.Sign.D.Cmp(d.Sign.D) != 0 {
		t.Fatal("signing scalar did not round-trip")
	}
	if !bytes.Equal(marshalSignPub(loaded.SignPub()), marshalSignPub(d.SignPub())) {
		t.Fatal("signing pubkey did not round-trip")
	}

	// The loaded device can actually open an envelope sealed to the original.
	env, _ := Seal([]byte("hi"), []X25519PubKey{d.X25519PubKey()}, AtRestAAD{AccountID: "a", SecretID: "s", Version: 1})
	got, err := Open(env, loaded.X25519Priv)
	if err != nil || string(got) != "hi" {
		t.Fatalf("loaded device failed to open: got %q err %v", got, err)
	}
}

func TestKeyfileRejectsGarbage(t *testing.T) {
	if _, err := ParseKeyfile([]byte("{not json")); err == nil {
		t.Fatal("expected parse error on garbage keyfile")
	}
	if _, err := ParseKeyfile([]byte(`{"version":2}`)); err == nil {
		t.Fatal("expected unsupported-version error")
	}
}

// Sanity that NewMnemonic yields a valid 24-word BIP-39 phrase.
func TestNewMnemonicValid(t *testing.T) {
	m, err := NewMnemonic()
	if err != nil {
		t.Fatalf("NewMnemonic: %v", err)
	}
	if !bip39.IsMnemonicValid(m) {
		t.Fatal("generated mnemonic is not valid BIP-39")
	}
}
