// Package seal — cross-language test vectors.
//
// TestGenerateAndVerifyVectors validates the golden JSON files in
// testdata/owner-seal/ in two modes:
//
//   - mnemonic_derivation.json is fully deterministic (no signatures): a strict
//     golden-compare ensures the derivation algorithm is stable.
//   - deviceset_chain.json contains non-deterministic ECDSA signatures, so it
//     cannot be regenerated bit-for-bit.  Normal runs READ the stored file and
//     verify the chain + hashes semantically.  Pass -update to regenerate
//     (using a pinned clock so body bytes are stable).
//
// The vitest reader (web/build/seal-vectors.test.ts) reads the same files and
// validates the hash algorithm from the TypeScript side.
//
// Files:
//
//	testdata/owner-seal/mnemonic_derivation.json  — BIP-39 seed → HKDF → key mapping
//	testdata/owner-seal/deviceset_chain.json      — genesis+add+remove chain with raw bytes and hashes
package seal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden vector files in testdata/owner-seal/")

// vectorsEnrolledAt is the fixed enrolled_at timestamp written into chain entry
// bodies when regenerating vectors (-update).  Stable body bytes guarantee that
// the head hashes in the stored file are reproducible across platforms (WM10).
const vectorsEnrolledAt = "1700000000000000000"

// Fixed BIP-39 spec test vectors (24-word, 256-bit entropy) — identical across
// Go and TypeScript for cross-language validation.
const (
	vectorMnemonicDevice1  = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"
	vectorMnemonicRecovery = "legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth title"
	vectorMnemonicDevice2  = "letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic bless"
)

// checkGolden compares v (marshalled as indented JSON) against the golden file
// at testdata/owner-seal/<name>.  When -update is set the file is rewritten.
// Use only for fully deterministic data; for data with random components (e.g.
// ECDSA signatures) write on -update and verify semantically on normal runs.
func checkGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	fpath := filepath.Join("testdata", "owner-seal", name)
	if *update {
		if err := os.WriteFile(fpath, append(got, '\n'), 0o600); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		t.Logf("updated golden: %s", fpath)
		return
	}
	stored, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", name, err)
	}
	if !bytes.Equal(got, bytes.TrimRight(stored, "\n")) {
		t.Fatalf("golden mismatch for %s; run `go test -run=TestGenerateAndVerifyVectors -update ./internal/secrets/seal/` to regenerate", name)
	}
}

// derivationVector is one row in mnemonic_derivation.json.
type derivationVector struct {
	Mnemonic   string `json:"mnemonic"`
	X25519Pub  string `json:"x25519_pub"`  // base64
	SignPub    string `json:"sign_pub"`    // base64 SEC1-uncompressed
	X25519Priv string `json:"x25519_priv"` // base64 (for TS re-derivation check)
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

// TestGenerateAndVerifyVectors is the golden-file driver.  See package comment
// for the two-part approach (strict golden vs semantic verify).
func TestGenerateAndVerifyVectors(t *testing.T) {
	d1, err := DeviceFromMnemonic(vectorMnemonicDevice1, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic device1: %v", err)
	}
	rec, err := DeviceFromMnemonic(vectorMnemonicRecovery, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic recovery: %v", err)
	}
	d2, err := DeviceFromMnemonic(vectorMnemonicDevice2, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic device2: %v", err)
	}

	// ── mnemonic_derivation.json (deterministic — strict golden compare) ──────

	devVectors := []derivationVector{
		{
			Mnemonic:   vectorMnemonicDevice1,
			X25519Pub:  base64.StdEncoding.EncodeToString(d1.Ref().X25519Pub),
			SignPub:    base64.StdEncoding.EncodeToString(marshalSignPub(d1.SignPub())),
			X25519Priv: base64.StdEncoding.EncodeToString(d1.X25519Priv),
		},
		{
			Mnemonic:   vectorMnemonicRecovery,
			X25519Pub:  base64.StdEncoding.EncodeToString(rec.Ref().X25519Pub),
			SignPub:    base64.StdEncoding.EncodeToString(marshalSignPub(rec.SignPub())),
			X25519Priv: base64.StdEncoding.EncodeToString(rec.X25519Priv),
		},
		{
			Mnemonic:   vectorMnemonicDevice2,
			X25519Pub:  base64.StdEncoding.EncodeToString(d2.Ref().X25519Pub),
			SignPub:    base64.StdEncoding.EncodeToString(marshalSignPub(d2.SignPub())),
			X25519Priv: base64.StdEncoding.EncodeToString(d2.X25519Priv),
		},
	}
	checkGolden(t, "mnemonic_derivation.json", devVectors)

	// ── deviceset_chain.json (ECDSA signatures are non-deterministic) ─────────
	//
	// -update: pin the clock so body bytes are stable, build a fresh chain with
	// new signatures, and write the golden file.
	// normal: read the committed golden file and verify it semantically.

	ownerRoot := OwnerRoot{
		Device1SignPub:  d1.Ref().SignPub,
		RecoverySignPub: rec.Ref().SignPub,
	}

	if *update {
		// Pin the clock so enrolled_at in each Body is reproducible (WM10).
		origNow := nowNanoStr
		nowNanoStr = func() string { return vectorsEnrolledAt }
		defer func() { nowNanoStr = origNow }()

		log, err := NewGenesis(d1, rec)
		if err != nil {
			t.Fatalf("NewGenesis: %v", err)
		}
		if err := log.AddDevice(d2.Ref(), d1); err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
		if err := log.RemoveDevice(d2.X25519Pub, d1); err != nil {
			t.Fatalf("RemoveDevice: %v", err)
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
		checkGolden(t, "deviceset_chain.json", cv)
	} else {
		verifyStoredChainVectors(t, ownerRoot)
	}

	// Re-verify derivation round-trip regardless of mode.
	for _, dv := range devVectors {
		d, err := DeviceFromMnemonic(dv.Mnemonic, "")
		if err != nil {
			t.Fatalf("re-derive %s...: %v", dv.Mnemonic[:20], err)
		}
		if base64.StdEncoding.EncodeToString(d.Ref().X25519Pub) != dv.X25519Pub {
			t.Fatalf("x25519_pub mismatch for %s...", dv.Mnemonic[:20])
		}
		if base64.StdEncoding.EncodeToString(d.Ref().SignPub) != dv.SignPub {
			t.Fatalf("sign_pub mismatch for %s...", dv.Mnemonic[:20])
		}
	}
}

// TestSealEnvelopeVector generates and/or verifies a full-envelope seal vector
// (testdata/owner-seal/seal_envelope.json).  The vector contains a Go-sealed
// envelope that the TypeScript test (hpke.test.ts) must be able to open.
//
// Because Go's rand.Reader is non-deterministic, the file is generated once
// with -update and checked in.  Normal runs verify Go can still Open it.
func TestSealEnvelopeVector(t *testing.T) {
	d1, err := DeviceFromMnemonic(vectorMnemonicDevice1, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic: %v", err)
	}

	const plaintext = "hello-ts-from-go"
	aad := AtRestAAD{AccountID: "acct-1", SecretID: "seal-vec-1", Version: 1}

	fpath := filepath.Join("testdata", "owner-seal", "seal_envelope.json")

	if *update {
		env, err := Seal([]byte(plaintext), []X25519PubKey{d1.X25519PubKey()}, aad)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		v := sealEnvelopeVector{
			RecipientMnemonic: vectorMnemonicDevice1,
			PlaintextB64:      base64.StdEncoding.EncodeToString([]byte(plaintext)),
			AccountID:         aad.AccountID,
			SecretID:          aad.SecretID,
			Version:           aad.Version,
			Envelope:          env,
		}
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatalf("marshal seal vector: %v", err)
		}
		if err := os.WriteFile(fpath, append(data, '\n'), 0o600); err != nil {
			t.Fatalf("write seal_envelope.json: %v", err)
		}
		t.Logf("updated seal_envelope.json")
		return
	}

	// Normal run: read and verify.
	data, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatalf("read seal_envelope.json (run with -update to create): %v", err)
	}
	var v sealEnvelopeVector
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal seal_envelope.json: %v", err)
	}
	wantD, _ := base64.StdEncoding.DecodeString(v.PlaintextB64)
	if string(wantD) != plaintext {
		t.Fatalf("seal_envelope.json plaintext mismatch: got %q want %q", string(wantD), plaintext)
	}
	got, err := Open(v.Envelope, d1.X25519Priv)
	if err != nil {
		t.Fatalf("Open(seal_envelope.json): %v", err)
	}
	if !bytes.Equal(got, wantD) {
		t.Fatalf("Open: got %q want %q", got, wantD)
	}
	t.Logf("seal_envelope.json verified OK")
}

// sealEnvelopeVector is the JSON structure for seal_envelope.json.
// Field names are snake_case so the TypeScript test can parse the file directly.
type sealEnvelopeVector struct {
	RecipientMnemonic string    `json:"recipient_mnemonic"`
	PlaintextB64      string    `json:"plaintext_b64"`
	AccountID         string    `json:"account_id"`
	SecretID          string    `json:"secret_id"`
	Version           uint64    `json:"version"`
	Envelope          *Envelope `json:"envelope"`
}

// verifyStoredChainVectors reads testdata/owner-seal/deviceset_chain.json and
// verifies:
//  1. The full chain passes VerifyDeviceSet (signatures and structure).
//  2. Each entry's stored head_hash matches the computed hash.
//
// This is the go test ./... path: it validates the stored vectors without
// regenerating them, so the test is stable even though ECDSA is non-deterministic.
func verifyStoredChainVectors(t *testing.T, ownerRoot OwnerRoot) {
	t.Helper()
	stored, err := os.ReadFile(filepath.Join("testdata", "owner-seal", "deviceset_chain.json"))
	if err != nil {
		t.Fatalf("read deviceset_chain.json: %v", err)
	}
	var cv chainVector
	if err := json.Unmarshal(stored, &cv); err != nil {
		t.Fatalf("unmarshal deviceset_chain.json: %v", err)
	}
	if len(cv.Entries) == 0 {
		t.Fatal("deviceset_chain.json has no entries")
	}

	// Decode each entry_bytes into a StoredEntry and build the log.
	var log DeviceSetLog
	for i, ev := range cv.Entries {
		raw, err := base64.StdEncoding.DecodeString(ev.EntryBytes)
		if err != nil {
			t.Fatalf("decode entry[%d]: %v", i, err)
		}
		var se StoredEntry
		if err := json.Unmarshal(raw, &se); err != nil {
			t.Fatalf("unmarshal entry[%d]: %v", i, err)
		}
		log.Entries = append(log.Entries, se)
	}

	// Full chain verification — confirms signatures, structure, and membership.
	if _, err := VerifyDeviceSet(&log, ownerRoot); err != nil {
		t.Fatalf("VerifyDeviceSet on stored vectors: %v", err)
	}

	// Hash consistency: the stored head_hash must equal the computed hash for
	// each entry.  This validates the encodeFields+SHA-256 algorithm that TS
	// must replicate.
	for i, e := range log.Entries {
		got, err := e.Hash()
		if err != nil {
			t.Fatalf("entry[%d].Hash: %v", i, err)
		}
		want, _ := base64.StdEncoding.DecodeString(cv.Entries[i].HeadHash)
		if !bytes.Equal(got, want) {
			t.Fatalf("entry[%d] head_hash mismatch (golden file corrupt?)", i)
		}
	}
	t.Logf("verified %d stored chain entries", len(log.Entries))
}
