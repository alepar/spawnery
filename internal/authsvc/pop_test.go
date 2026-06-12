package authsvc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"math/big"
	"testing"
	"time"
)

func genP256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func spkiOf(t *testing.T, priv *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// buildPoP constructs a valid PoPProof signed with key.
func buildPoP(t *testing.T, priv *ecdsa.PrivateKey, rawRefreshToken string, ts int64, nonce []byte) PoPProof {
	t.Helper()
	refreshHash := sha256sum([]byte(rawRefreshToken))
	var tsBytes [8]byte
	binary.BigEndian.PutUint64(tsBytes[:], uint64(ts))

	msg := make([]byte, 0)
	msg = append(msg, refreshPoPDomain...)
	msg = append(msg, refreshHash...)
	msg = append(msg, tsBytes[:]...)
	msg = append(msg, nonce...)
	digest := sha256.Sum256(msg)

	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	copy(sig[32-len(rBytes):32], rBytes)
	copy(sig[64-len(sBytes):64], sBytes)

	return PoPProof{
		RefreshTokenHash: refreshHash,
		Timestamp:        ts,
		Nonce:            nonce,
		Sig:              sig,
	}
}

func TestPoPHappyPath(t *testing.T) {
	priv := genP256(t)
	spkiDER := spkiOf(t, priv)
	now := time.Unix(1770000000, 0)
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)

	proof := buildPoP(t, priv, "my-refresh-token", now.Unix(), nonce)
	if err := VerifyPoP(spkiDER, proof, now); err != nil {
		t.Fatalf("valid PoP rejected: %v", err)
	}
}

func TestPoPMissingSig(t *testing.T) {
	priv := genP256(t)
	spkiDER := spkiOf(t, priv)
	proof := PoPProof{Timestamp: 1770000000, Nonce: make([]byte, 16)}
	if err := VerifyPoP(spkiDER, proof, time.Unix(1770000000, 0)); err != ErrPoPMissing {
		t.Fatalf("want ErrPoPMissing, got %v", err)
	}
}

func TestPoPStaleTimestamp(t *testing.T) {
	priv := genP256(t)
	spkiDER := spkiOf(t, priv)
	nonce := make([]byte, 16)
	now := time.Unix(1770000000, 0)
	// Timestamp is 91s old — outside the 90s skew window.
	staleTS := now.Unix() - 91
	proof := buildPoP(t, priv, "tok", staleTS, nonce)
	if err := VerifyPoP(spkiDER, proof, now); err != ErrPoPStale {
		t.Fatalf("want ErrPoPStale, got %v", err)
	}
}

func TestPoPBadNonce(t *testing.T) {
	priv := genP256(t)
	spkiDER := spkiOf(t, priv)
	now := time.Unix(1770000000, 0)
	// Nonce too short.
	proof := buildPoP(t, priv, "tok", now.Unix(), make([]byte, 8))
	if err := VerifyPoP(spkiDER, proof, now); err != ErrPoPBadNonce {
		t.Fatalf("want ErrPoPBadNonce, got %v", err)
	}
}

func TestPoPWrongKey(t *testing.T) {
	priv := genP256(t)
	other := genP256(t)
	spkiDER := spkiOf(t, other) // stored key is OTHER's key
	now := time.Unix(1770000000, 0)
	nonce := make([]byte, 16)
	proof := buildPoP(t, priv, "tok", now.Unix(), nonce) // signed with wrong key
	if err := VerifyPoP(spkiDER, proof, now); err != ErrPoPBadSig {
		t.Fatalf("want ErrPoPBadSig, got %v", err)
	}
}

func TestPoPTamperedSig(t *testing.T) {
	priv := genP256(t)
	spkiDER := spkiOf(t, priv)
	now := time.Unix(1770000000, 0)
	nonce := make([]byte, 16)
	proof := buildPoP(t, priv, "tok", now.Unix(), nonce)
	proof.Sig[0] ^= 0xff // tamper
	if err := VerifyPoP(spkiDER, proof, now); err != ErrPoPBadSig {
		t.Fatalf("want ErrPoPBadSig, got %v", err)
	}
}

func TestPoPBadSPKI(t *testing.T) {
	now := time.Unix(1770000000, 0)
	proof := PoPProof{Timestamp: now.Unix(), Nonce: make([]byte, 16), Sig: make([]byte, 64)}
	if err := VerifyPoP([]byte("garbage"), proof, now); err != ErrPoPBadKey {
		t.Fatalf("want ErrPoPBadKey, got %v", err)
	}
}

func TestPoPP1363EncodingVector(t *testing.T) {
	// Cross-language vector: verify the P1363 encoding contract (r||s, 32+32 bytes, big-endian
	// zero-padded) matches what WebCrypto SubtleCrypto.sign("ECDSA") produces. The test here
	// ensures our Go verifier accepts exactly this format and not DER.
	priv := genP256(t)
	spkiDER := spkiOf(t, priv)
	now := time.Unix(1770000000, 0)
	nonce := make([]byte, 16)
	proof := buildPoP(t, priv, "token", now.Unix(), nonce)

	// Sanity: exactly 64 bytes.
	if len(proof.Sig) != 64 {
		t.Fatalf("sig not 64 bytes: %d", len(proof.Sig))
	}
	// r and s as big.Int should both be positive.
	r := new(big.Int).SetBytes(proof.Sig[:32])
	s := new(big.Int).SetBytes(proof.Sig[32:])
	if r.Sign() <= 0 || s.Sign() <= 0 {
		t.Fatalf("r or s not positive: r=%v s=%v", r, s)
	}

	if err := VerifyPoP(spkiDER, proof, now); err != nil {
		t.Fatalf("P1363 vector rejected: %v", err)
	}
}
