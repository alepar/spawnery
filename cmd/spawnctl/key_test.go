package main

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
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

// TestRecoverFlow verifies the full local recovery flow: init produces a recovery
// mnemonic; recoverDevice accepts it, writes a fresh 0600 keyfile, rotates the
// recovery phrase, and leaves the device set with 3 members (device1 + fresh +
// new_recovery).
func TestRecoverFlow(t *testing.T) {
	dir := t.TempDir()

	// init creates device1 + recovery (2 members).
	recoveryMnemonic, err := initDeviceSet(dir, false)
	if err != nil {
		t.Fatalf("initDeviceSet: %v", err)
	}

	// Remove the device1 keyfile so recoverDevice can write the fresh device's keyfile.
	if err := os.Remove(keyfilePath(dir)); err != nil {
		t.Fatalf("remove keyfile before recover: %v", err)
	}

	result, err := recoverDevice(dir, recoveryMnemonic, false)
	if err != nil {
		t.Fatalf("recoverDevice: %v", err)
	}

	// FreshDevice must have non-empty key material.
	freshDev := result.FreshDevice
	if len(freshDev.X25519Pub) == 0 || freshDev.Sign == nil {
		t.Fatal("FreshDevice missing key material")
	}

	// NewRecoveryMnemonic must be a valid 24-word phrase distinct from the original.
	newPhrase := result.NewRecoveryMnemonic
	if !strings.Contains(newPhrase, " ") || len(strings.Fields(newPhrase)) != 24 {
		t.Fatalf("NewRecoveryMnemonic is not 24 words: %q", newPhrase)
	}
	if newPhrase == recoveryMnemonic {
		t.Fatal("NewRecoveryMnemonic must differ from the consumed recovery mnemonic")
	}

	// The written keyfile must be 0600.
	fi, err := os.Stat(keyfilePath(dir))
	if err != nil {
		t.Fatalf("stat recovered keyfile: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keyfile perm = %o, want 0600", perm)
	}

	// The device set must verify and contain exactly 3 members:
	// device1 (from genesis) + fresh device + new recovery virtual device.
	dsf, err := loadDeviceSet(dir)
	if err != nil {
		t.Fatalf("loadDeviceSet after recovery: %v", err)
	}
	members, err := seal.VerifyDeviceSet(dsf.Log, dsf.Root)
	if err != nil {
		t.Fatalf("VerifyDeviceSet after recovery: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("members after recovery = %d, want 3 (device1 + fresh + new_recovery)", len(members))
	}
}

func TestRecoverInvalidMnemonic(t *testing.T) {
	if _, err := recoverDevice(t.TempDir(), "not a valid phrase", false); err == nil {
		t.Fatal("expected invalid mnemonic to error")
	}
}

func TestRecoverNoClobber(t *testing.T) {
	// Case 1: keyfile present, --force not set → must fail.
	{
		dir := t.TempDir()
		recoveryMnemonic, err := initDeviceSet(dir, false)
		if err != nil {
			t.Fatalf("initDeviceSet: %v", err)
		}
		// init writes a device1 keyfile; recovery without --force must reject it.
		if _, err := recoverDevice(dir, recoveryMnemonic, false); err == nil {
			t.Fatal("recover without --force should fail when keyfile exists")
		}
	}

	// Case 2: keyfile present, --force set → must succeed.
	{
		dir := t.TempDir()
		recoveryMnemonic, err := initDeviceSet(dir, false)
		if err != nil {
			t.Fatalf("initDeviceSet: %v", err)
		}
		// Device1 keyfile exists; --force must overwrite it with the fresh device.
		if _, err := recoverDevice(dir, recoveryMnemonic, true); err != nil {
			t.Fatalf("recover --force should overwrite existing keyfile: %v", err)
		}
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

// sasFmt matches the "xxxx-xxxx-xxxx" 3×4 base-36 output format.
var sasFmt = regexp.MustCompile(`^[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}$`)

// TestApproveDeviceSAS verifies that approveDevice independently derives the
// SAS from the chain state and the new device's pubkeys (spec §2 [WM4]).
// The returned SAS must be in the expected 3×4 base-36 format and must be
// non-empty so the CLI can display it for human comparison.
func TestApproveDeviceSAS(t *testing.T) {
	dir := t.TempDir()
	if _, err := initDeviceSet(dir, false); err != nil {
		t.Fatalf("initDeviceSet: %v", err)
	}

	// Build a minimal enrollment payload for a fresh second device.
	freshDev, err := seal.NewMnemonic()
	if err != nil {
		t.Fatalf("NewMnemonic: %v", err)
	}
	enrollDev, err := seal.DeviceFromMnemonic(freshDev, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic: %v", err)
	}
	ref := enrollDev.Ref()

	payload, err := json.Marshal(map[string]string{
		"x25519Pub":  encodeBase64(ref.X25519Pub),
		"signPub":    encodeBase64(ref.SignPub),
		"deviceName": "test-device",
		"expiresAt":  "2099-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	result, err := approveDevice(dir, string(payload), "test-device")
	if err != nil {
		t.Fatalf("approveDevice: %v", err)
	}

	// SAS must be in the correct format.
	if !sasFmt.MatchString(result.SAS) {
		t.Fatalf("approveDevice SAS format wrong: %q (want xxxx-xxxx-xxxx base-36)", result.SAS)
	}
	// Response must contain the ownerRoot fields.
	if !strings.Contains(result.Response, "ownerRoot") {
		t.Fatalf("approveDevice Response missing ownerRoot: %s", result.Response)
	}
}

// TestApproveDeviceSASMITM verifies that approving two different enrollment
// payloads with different x25519 pubkeys produces different SAS codes — a
// MITM that substitutes a pubkey must be detectable via the SAS (spec §2 [WM4]).
func TestApproveDeviceSASMITM(t *testing.T) {
	// Initialize a device set on a temp dir.
	dir := t.TempDir()
	if _, err := initDeviceSet(dir, false); err != nil {
		t.Fatalf("initDeviceSet: %v", err)
	}

	makePayload := func(x25519 []byte, signPub []byte) string {
		p, _ := json.Marshal(map[string]string{
			"x25519Pub":  encodeBase64(x25519),
			"signPub":    encodeBase64(signPub),
			"deviceName": "dev",
			"expiresAt":  "2099-01-01T00:00:00Z",
		})
		return string(p)
	}

	dev1, _ := seal.DeviceFromMnemonic(testMnemonic, "")
	ref1 := dev1.Ref()

	mn2, _ := seal.NewMnemonic()
	dev2, _ := seal.DeviceFromMnemonic(mn2, "")
	ref2 := dev2.Ref()

	// Approve the legitimate device first to get its SAS.
	legitResult, err := approveDevice(dir, makePayload(ref1.X25519Pub, ref1.SignPub), "legit")
	if err != nil {
		t.Fatalf("approveDevice legit: %v", err)
	}

	// Re-initialise with a fresh device set for the MITM comparison (so the
	// chain state is the same as the first approval).
	dir2 := t.TempDir()
	if _, err := initDeviceSet(dir2, false); err != nil {
		t.Fatalf("initDeviceSet MITM: %v", err)
	}
	// Approve with the MITM's substituted x25519 pubkey.
	mitmResult, err := approveDevice(dir2, makePayload(ref2.X25519Pub, ref1.SignPub), "mitm")
	if err != nil {
		t.Fatalf("approveDevice MITM: %v", err)
	}

	if legitResult.SAS == mitmResult.SAS {
		t.Fatalf("MITM failure: substituted x25519 pubkey produced identical SAS %q — "+
			"a MITM'd enrollment would pass human comparison undetected", legitResult.SAS)
	}
}

// TestM8WarningVerbatim asserts the CLI constant reproduces the verbatim M8
// banner copy from the owner-sealed spec §3 (spec [WM12]).
func TestM8WarningVerbatim(t *testing.T) {
	const specText = "approve from your phone / enter recovery code only on a trusted device"
	if !strings.Contains(m8TrustedDeviceWarning, specText) {
		t.Fatalf("m8TrustedDeviceWarning does not contain the verbatim spec §3 banner copy %q;\n"+
			"got: %q", specText, m8TrustedDeviceWarning)
	}
}
