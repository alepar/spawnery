package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"spawnery/internal/secrets/seal"
)

// testMnemonic is the canonical all-zero-entropy 24-word BIP-39 vector (valid
// checksum word "art"). Using a fixed mnemonic lets us assert deterministic
// derivation at the CLI layer.
const testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"

// init writes a 0600 device keyfile and a verifiable genesis device-set.
func TestInitDeviceSet(t *testing.T) {
	dir := t.TempDir()

	recovery, err := initDeviceSet(dir, false)
	if err != nil {
		t.Fatalf("initDeviceSet: %v", err)
	}
	if !strings.Contains(recovery, " ") || len(strings.Fields(recovery)) != 24 {
		t.Fatalf("recovery mnemonic is not a 24-word phrase: %q", recovery)
	}

	// Keyfile exists and is 0600.
	fi, err := os.Stat(keyfilePath(dir))
	if err != nil {
		t.Fatalf("stat keyfile: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keyfile perm = %o, want 0600", perm)
	}

	// The keyfile round-trips into a usable device.
	dev, err := loadDevice(dir)
	if err != nil {
		t.Fatalf("loadDevice: %v", err)
	}
	if len(dev.X25519Pub) == 0 || dev.Sign == nil {
		t.Fatal("loaded device missing key material")
	}

	// The genesis device-set verifies against its pinned root and resolves to
	// exactly two members (device1 + recovery).
	dsf, err := loadDeviceSet(dir)
	if err != nil {
		t.Fatalf("loadDeviceSet: %v", err)
	}
	members, err := seal.VerifyDeviceSet(dsf.Log, dsf.Root)
	if err != nil {
		t.Fatalf("VerifyDeviceSet: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("genesis members = %d, want 2 (device1 + recovery)", len(members))
	}

	// device1's signing pubkey is pinned as the owner root.
	if !bytes.Equal(dsf.Root.Device1SignPub, dev.Ref().SignPub) {
		t.Fatal("pinned device1 root does not match the written keyfile")
	}
}

// init refuses to clobber an existing keyfile unless forced.
func TestInitDeviceSetNoClobber(t *testing.T) {
	dir := t.TempDir()
	if _, err := initDeviceSet(dir, false); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if _, err := initDeviceSet(dir, false); err == nil {
		t.Fatal("second init without --force should have failed")
	}
	if _, err := initDeviceSet(dir, true); err != nil {
		t.Fatalf("init --force should overwrite: %v", err)
	}
}

// recover re-derives the SAME device from a fixed mnemonic every time.
func TestRecoverDeterministic(t *testing.T) {
	want, err := seal.RecoveryDevice(testMnemonic)
	if err != nil {
		t.Fatalf("reference RecoveryDevice: %v", err)
	}

	dirs := []string{t.TempDir(), t.TempDir()}
	var firstPub []byte
	for i, dir := range dirs {
		dev, err := recoverDevice(dir, testMnemonic, false)
		if err != nil {
			t.Fatalf("recoverDevice[%d]: %v", i, err)
		}
		if !bytes.Equal(dev.X25519Pub, want.X25519Pub) {
			t.Fatalf("recoverDevice[%d] X25519 pub mismatch vs reference", i)
		}
		if !bytes.Equal(dev.Ref().SignPub, want.Ref().SignPub) {
			t.Fatalf("recoverDevice[%d] sign pub mismatch vs reference", i)
		}
		// Perms on the recovered keyfile.
		fi, err := os.Stat(keyfilePath(dir))
		if err != nil {
			t.Fatalf("stat recovered keyfile: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("recovered keyfile perm = %o, want 0600", perm)
		}
		if i == 0 {
			firstPub = dev.X25519Pub
		} else if !bytes.Equal(firstPub, dev.X25519Pub) {
			t.Fatal("two recoveries from the same mnemonic differ")
		}
	}
}

func TestRecoverInvalidMnemonic(t *testing.T) {
	if _, err := recoverDevice(t.TempDir(), "not a valid phrase", false); err == nil {
		t.Fatal("expected invalid mnemonic to error")
	}
}

func TestRecoverNoClobber(t *testing.T) {
	dir := t.TempDir()
	if _, err := recoverDevice(dir, testMnemonic, false); err != nil {
		t.Fatalf("first recover: %v", err)
	}
	if _, err := recoverDevice(dir, testMnemonic, false); err == nil {
		t.Fatal("recover over existing keyfile without --force should fail")
	}
	if _, err := recoverDevice(dir, testMnemonic, true); err != nil {
		t.Fatalf("recover --force should overwrite: %v", err)
	}
}

// loaders return actionable errors when nothing has been initialized.
func TestLoadMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadDevice(dir); err == nil || !strings.Contains(err.Error(), "key init") {
		t.Fatalf("loadDevice on empty dir: %v", err)
	}
	if _, err := loadDeviceSet(dir); err == nil || !strings.Contains(err.Error(), "key init") {
		t.Fatalf("loadDeviceSet on empty dir: %v", err)
	}
}

// deviceSetFile JSON round-trips (root + log) so device-set show reads what init wrote.
func TestDeviceSetFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := initDeviceSet(dir, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	dsf, err := loadDeviceSet(dir)
	if err != nil {
		t.Fatalf("loadDeviceSet: %v", err)
	}
	if len(dsf.Root.Device1SignPub) == 0 || len(dsf.Root.RecoverySignPub) == 0 {
		t.Fatal("owner root did not round-trip through JSON")
	}
	if dsf.Log == nil || len(dsf.Log.Entries) != 1 {
		t.Fatalf("device-set log did not round-trip: %+v", dsf.Log)
	}
}

func TestFormatDeviceSetShow(t *testing.T) {
	dir := t.TempDir()
	if _, err := initDeviceSet(dir, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	dsf, err := loadDeviceSet(dir)
	if err != nil {
		t.Fatalf("loadDeviceSet: %v", err)
	}

	out := formatDeviceSetShow(dsf)
	if !strings.Contains(out, "chain: VALID") {
		t.Fatalf("expected VALID chain, got:\n%s", out)
	}
	if !strings.Contains(out, "members (2)") {
		t.Fatalf("expected 2 members, got:\n%s", out)
	}
	if strings.Count(out, "SP-") != 2 {
		t.Fatalf("expected two device fingerprints, got:\n%s", out)
	}

	// Tamper: a flipped signing-root byte must turn the verdict INVALID.
	bad := &deviceSetFile{Root: dsf.Root, Log: dsf.Log}
	bad.Root.Device1SignPub = append([]byte(nil), dsf.Root.Device1SignPub...)
	bad.Root.Device1SignPub[len(bad.Root.Device1SignPub)-1] ^= 0xff
	badOut := formatDeviceSetShow(bad)
	if !strings.Contains(badOut, "chain: INVALID") {
		t.Fatalf("expected INVALID chain on tampered root, got:\n%s", badOut)
	}
}

// formatKeyShow prints public identity + fingerprint and never the private key.
func TestFormatKeyShowNoSecrets(t *testing.T) {
	dev, err := seal.RecoveryDevice(testMnemonic)
	if err != nil {
		t.Fatalf("RecoveryDevice: %v", err)
	}
	out := formatKeyShow(dev)
	if !strings.Contains(out, "fingerprint:") || !strings.Contains(out, "SP-") {
		t.Fatalf("missing fingerprint:\n%s", out)
	}
	// The X25519 private scalar must never appear in the rendered output.
	privHex := strings.ToLower(bytesToHex(dev.X25519Priv))
	if strings.Contains(strings.ToLower(out), privHex) {
		t.Fatal("formatKeyShow leaked the X25519 private key")
	}
}

// fingerprint is stable for the same device and differs across devices.
func TestDeviceFingerprintStable(t *testing.T) {
	a, _ := seal.RecoveryDevice(testMnemonic)
	if got1, got2 := deviceFingerprint(a.Ref()), deviceFingerprint(a.Ref()); got1 != got2 {
		t.Fatalf("fingerprint not stable: %q vs %q", got1, got2)
	}
	b, err := seal.NewMnemonic()
	if err != nil {
		t.Fatalf("NewMnemonic: %v", err)
	}
	bdev, _ := seal.DeviceFromMnemonic(b, "")
	if deviceFingerprint(a.Ref()) == deviceFingerprint(bdev.Ref()) {
		t.Fatal("distinct devices produced the same fingerprint")
	}
}

func TestFormatInitResultCarriesWarning(t *testing.T) {
	out := formatInitResult("/tmp/x", testMnemonic)
	if !strings.Contains(out, "cannot be recovered by anyone, including Spawnery") {
		t.Fatalf("init output missing the mandatory loss warning:\n%s", out)
	}
	if !strings.Contains(out, testMnemonic) {
		t.Fatal("init output did not include the recovery mnemonic")
	}
}

func bytesToHex(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}
