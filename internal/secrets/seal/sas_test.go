package seal

import (
	"regexp"
	"testing"
)

// sasFmt matches the "xxxx-xxxx-xxxx" 3×4 base-36 output format.
var sasFmt = regexp.MustCompile(`^[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}$`)

func TestDeriveSASFormat(t *testing.T) {
	genesis := []byte("genesis-hash-test")
	head := []byte("head-hash-test")
	x25519 := []byte("new-x25519-pub-32bytes-padded-xx")
	sign := []byte("new-sign-pub-65bytes-padded-xxxx")

	got, err := DeriveSAS(genesis, head, x25519, sign)
	if err != nil {
		t.Fatalf("DeriveSAS: %v", err)
	}
	if !sasFmt.MatchString(got) {
		t.Fatalf("SAS format wrong: %q (want xxxx-xxxx-xxxx base-36)", got)
	}
	t.Logf("SAS: %s", got)
}

func TestDeriveSASStability(t *testing.T) {
	genesis := []byte("genesis-hash-test")
	head := []byte("head-hash-test")
	x25519 := []byte("new-x25519-pub-32bytes-padded-xx")
	sign := []byte("new-sign-pub-65bytes-padded-xxxx")

	a, err := DeriveSAS(genesis, head, x25519, sign)
	if err != nil {
		t.Fatalf("first DeriveSAS: %v", err)
	}
	b, err := DeriveSAS(genesis, head, x25519, sign)
	if err != nil {
		t.Fatalf("second DeriveSAS: %v", err)
	}
	if a != b {
		t.Fatalf("DeriveSAS not stable: %q vs %q", a, b)
	}
}

// TestDeriveSASMITMSubstitutedPubkey is the spec §4 named negative: a MITM that
// substitutes the new device's x25519 public key MUST produce a different SAS,
// so the human comparison catches the attack (spec §2 [WM4]).  If the SAS were
// the same for a substituted key, an active attacker could swap the pubkey in
// transit and enroll their own device undetected.
func TestDeriveSASMITMSubstitutedPubkey(t *testing.T) {
	genesis := []byte("genesis-hash-test")
	head := []byte("head-hash-test")
	legitX25519 := []byte("legit-x25519-pub-32bytes-paddxxx")
	sign := []byte("new-sign-pub-65bytes-padded-xxxx")

	legitimate, err := DeriveSAS(genesis, head, legitX25519, sign)
	if err != nil {
		t.Fatalf("DeriveSAS legitimate: %v", err)
	}

	// Attacker substitutes a different x25519 pubkey.
	mitmX25519 := []byte("mitm--x25519-pub-32bytes-paddxxx")
	mitmSAS, err := DeriveSAS(genesis, head, mitmX25519, sign)
	if err != nil {
		t.Fatalf("DeriveSAS MITM: %v", err)
	}
	if legitimate == mitmSAS {
		t.Fatalf("MITM failure: substituted x25519 pubkey produced identical SAS %q — "+
			"a MITM'd link would pass the human comparison undetected", legitimate)
	}
}

// TestDeriveSASMITMSubstitutedSignPub mirrors the x25519 test for the signing
// pubkey; both pubkeys appear in the SAS preimage so both must be validated.
func TestDeriveSASMITMSubstitutedSignPub(t *testing.T) {
	genesis := []byte("genesis-hash-test")
	head := []byte("head-hash-test")
	x25519 := []byte("new-x25519-pub-32bytes-padded-xx")
	legitSign := []byte("legit-sign-pub-65bytes-padded-xx")

	legitimate, err := DeriveSAS(genesis, head, x25519, legitSign)
	if err != nil {
		t.Fatalf("DeriveSAS legitimate: %v", err)
	}

	mitmSign := []byte("mitm--sign-pub-65bytes-padded-xx")
	mitmSAS, err := DeriveSAS(genesis, head, x25519, mitmSign)
	if err != nil {
		t.Fatalf("DeriveSAS MITM sign: %v", err)
	}
	if legitimate == mitmSAS {
		t.Fatalf("MITM failure: substituted sign pubkey produced identical SAS %q", legitimate)
	}
}

// TestDeriveSASEmptyInputsRejected confirms the guard against zero-length hashes.
func TestDeriveSASEmptyInputsRejected(t *testing.T) {
	x25519 := []byte("x25519pub")
	sign := []byte("signpub")

	if _, err := DeriveSAS(nil, []byte("head"), x25519, sign); err == nil {
		t.Fatal("expected error for nil genesis_hash")
	}
	if _, err := DeriveSAS([]byte("genesis"), nil, x25519, sign); err == nil {
		t.Fatal("expected error for nil head_hash")
	}
	if _, err := DeriveSAS(nil, nil, x25519, sign); err == nil {
		t.Fatal("expected error for both nil hashes")
	}
}

// TestDeriveSASKnownVector is the cross-language golden-value test.
// Both sides use the same fixed ASCII byte inputs; the expected SAS was computed
// once and is locked here.  If this test fails the algorithm diverged.
//
// Corresponding TS test: web/src/keys/sas.test.ts "cross-language known vector"
//
// To verify the hardcoded value, run the computation manually:
//   encodeFields("sas/v1", genesis, head, x25519, sign) → SHA-256 → base-36[0:12]
func TestDeriveSASKnownVector(t *testing.T) {
	// Fixed ASCII inputs — stable across platforms and test runs.
	genesis := []byte("test-genesis-hash")
	head := []byte("test-head-hash")
	x25519 := []byte("test-x25519-pub")
	sign := []byte("test-sign-pub")

	const wantSAS = "0004-53td-gr6k"

	got, err := DeriveSAS(genesis, head, x25519, sign)
	if err != nil {
		t.Fatalf("DeriveSAS: %v", err)
	}
	if got != wantSAS {
		t.Fatalf("DeriveSAS known-vector mismatch: got %q, want %q", got, wantSAS)
	}
}
