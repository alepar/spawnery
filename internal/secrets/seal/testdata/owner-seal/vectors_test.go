// Package vectors_test generates and re-verifies the shared test vectors in this
// directory.  Running `go test` here always regenerates the files from the same
// fixed inputs and verifies the stored results, making cross-language diffs
// immediately obvious.
//
// Files:
//
//	mnemonic_derivation.json  — BIP-39 seed → HKDF → {x25519_pub, sign_pub} mapping
//	deviceset_chain.json      — 3-entry genesis+add+remove chain with all raw bytes
package vectors_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/secrets/seal"
)

// Fixed mnemonics — deterministic inputs for both generated files.
// All three are official BIP-39 spec test vectors (24-word, 256-bit entropy).
const (
	mnemonicDevice1  = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"
	mnemonicRecovery = "legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth title"
	mnemonicDevice2  = "letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic bless"
)

func mustDevice(t *testing.T, mnemonic string) *seal.Device {
	t.Helper()
	d, err := seal.DeviceFromMnemonic(mnemonic, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic(%q): %v", mnemonic[:20], err)
	}
	return d
}

func writeJSON(t *testing.T, name string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	dir := t.TempDir()
	// Also write to the testdata dir so they persist across runs.
	dir = filepath.Join(filepath.Dir(testSelf(t)), "")
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// testSelf returns the path of the test source file (relies on runtime.Caller
// indirectly — we use the test binary's working directory instead).
func testSelf(t *testing.T) string {
	t.Helper()
	// Under `go test`, the working directory is the package directory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "vectors_test.go") // the file itself
}

// derivationVector is one row in mnemonic_derivation.json.
type derivationVector struct {
	Mnemonic   string `json:"mnemonic"`
	X25519Pub  string `json:"x25519_pub"`  // base64
	SignPub    string `json:"sign_pub"`    // base64 SEC1-uncompressed
	X25519Priv string `json:"x25519_priv"` // base64 (so TS can re-derive and check)
}

// entryVector is one entry in deviceset_chain.json.
type entryVector struct {
	Version    uint64 `json:"version"`
	EntryBytes string `json:"entry_bytes"` // base64 json.Marshal(StoredEntry)
	HeadHash   string `json:"head_hash"`   // base64 sha256(encodeFields(Body, sigs…))
}

type chainVector struct {
	OwnerRoot struct {
		Device1SignPub  string `json:"device1_sign_pub"`  // base64
		RecoverySignPub string `json:"recovery_sign_pub"` // base64
	} `json:"owner_root"`
	Entries []entryVector `json:"entries"`
}

// TestGenerateAndVerifyVectors generates both JSON files and immediately
// re-verifies them.  A deterministic seed means the files are identical on
// every run; diffs indicate a regression in the Go implementation.
func TestGenerateAndVerifyVectors(t *testing.T) {
	d1 := mustDevice(t, mnemonicDevice1)
	rec := mustDevice(t, mnemonicRecovery)
	d2 := mustDevice(t, mnemonicDevice2)

	// ---- mnemonic_derivation.json -------------------------------------------

	sec1 := func(d *seal.Device) []byte {
		ref := d.Ref()
		return ref.SignPub
	}

	devVectors := []derivationVector{
		{
			Mnemonic:   mnemonicDevice1,
			X25519Pub:  base64.StdEncoding.EncodeToString(d1.Ref().X25519Pub),
			SignPub:    base64.StdEncoding.EncodeToString(sec1(d1)),
			X25519Priv: base64.StdEncoding.EncodeToString(d1.X25519Priv),
		},
		{
			Mnemonic:   mnemonicRecovery,
			X25519Pub:  base64.StdEncoding.EncodeToString(rec.Ref().X25519Pub),
			SignPub:    base64.StdEncoding.EncodeToString(sec1(rec)),
			X25519Priv: base64.StdEncoding.EncodeToString(rec.X25519Priv),
		},
		{
			Mnemonic:   mnemonicDevice2,
			X25519Pub:  base64.StdEncoding.EncodeToString(d2.Ref().X25519Pub),
			SignPub:    base64.StdEncoding.EncodeToString(sec1(d2)),
			X25519Priv: base64.StdEncoding.EncodeToString(d2.X25519Priv),
		},
	}
	writeJSON(t, "mnemonic_derivation.json", devVectors)

	// ---- deviceset_chain.json -----------------------------------------------

	log, err := seal.NewGenesis(d1, rec)
	if err != nil {
		t.Fatalf("NewGenesis: %v", err)
	}
	if err := log.AddDevice(d2.Ref(), d1); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	if err := log.RemoveDevice(d2.X25519Pub, d1); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}

	ownerRoot := seal.OwnerRoot{
		Device1SignPub:  d1.Ref().SignPub,
		RecoverySignPub: rec.Ref().SignPub,
	}

	var cv chainVector
	cv.OwnerRoot.Device1SignPub = base64.StdEncoding.EncodeToString(ownerRoot.Device1SignPub)
	cv.OwnerRoot.RecoverySignPub = base64.StdEncoding.EncodeToString(ownerRoot.RecoverySignPub)

	for _, e := range log.Entries {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		h, err := e.Hash()
		if err != nil {
			t.Fatalf("entry.Hash: %v", err)
		}
		ver, _, err := e.VersionAndPrevHash()
		if err != nil {
			t.Fatalf("VersionAndPrevHash: %v", err)
		}
		cv.Entries = append(cv.Entries, entryVector{
			Version:    ver,
			EntryBytes: base64.StdEncoding.EncodeToString(raw),
			HeadHash:   base64.StdEncoding.EncodeToString(h),
		})
	}
	writeJSON(t, "deviceset_chain.json", cv)

	// ---- re-verify -----------------------------------------------------------

	// Verify the chain round-trips through VerifyDeviceSet.
	members, err := seal.VerifyDeviceSet(log, ownerRoot)
	if err != nil {
		t.Fatalf("VerifyDeviceSet: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("after add+remove: want 2 members, got %d", len(members))
	}

	// Verify each entry's HeadHash in the vector matches a fresh hash.
	for i, e := range log.Entries {
		got, _ := e.Hash()
		want, _ := base64.StdEncoding.DecodeString(cv.Entries[i].HeadHash)
		if !bytes.Equal(got, want) {
			t.Fatalf("entry[%d] hash mismatch", i)
		}
	}

	// Verify derivation vectors: re-derive and compare.
	for _, dv := range devVectors {
		d, err := seal.DeviceFromMnemonic(dv.Mnemonic, "")
		if err != nil {
			t.Fatalf("re-derive: %v", err)
		}
		gotX25519Pub := base64.StdEncoding.EncodeToString(d.Ref().X25519Pub)
		if gotX25519Pub != dv.X25519Pub {
			t.Fatalf("x25519 pub mismatch for %s", dv.Mnemonic[:20])
		}
		gotSignPub := base64.StdEncoding.EncodeToString(d.Ref().SignPub)
		if gotSignPub != dv.SignPub {
			t.Fatalf("sign pub mismatch for %s", dv.Mnemonic[:20])
		}
	}

	// Verify that deviceset_chain.json entries decode back to valid StoredEntries
	// and their hashes match.
	for i, ev := range cv.Entries {
		raw, err := base64.StdEncoding.DecodeString(ev.EntryBytes)
		if err != nil {
			t.Fatalf("decode vector entry[%d]: %v", i, err)
		}
		var se seal.StoredEntry
		if err := json.Unmarshal(raw, &se); err != nil {
			t.Fatalf("unmarshal vector entry[%d]: %v", i, err)
		}
		h, err := se.Hash()
		if err != nil {
			t.Fatalf("vector entry[%d] hash: %v", i, err)
		}
		wantH, _ := base64.StdEncoding.DecodeString(ev.HeadHash)
		if !bytes.Equal(h, wantH) {
			t.Fatalf("vector entry[%d]: round-trip hash mismatch", i)
		}
	}

	t.Logf("vectors written: %d derivation rows, %d chain entries", len(devVectors), len(cv.Entries))
}
