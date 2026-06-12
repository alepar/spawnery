package token

import (
	"crypto/ed25519"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
)

func mustMarshal(t *testing.T, body *authv1.SessionTokenBody) []byte {
	t.Helper()
	b, err := proto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Golden cross-language vector (Go side; A2/A5 TS reproduces it). The seed, body, and frozen
// wire string below are the shared contract — do NOT regenerate casually: a change here is a
// breaking re-spec of the token wire format [MC1].
const (
	vectorSeed = "0123456789abcdef0123456789abcdef" // 32 bytes, ed25519 seed
	vectorWire = "CgZhY2N0LTESBWFsaWNlGgV0b2stMSICY3AogOLPqgYwhOnPqgY6IDgadQQQ5Q8hCQlvPx1Hhni9qffGKP2-9FC7uhkdI33lQhBkZmNjYmZmYmYwZjhiODFk._fNLkv5Q7CygwNRWIkjuUV7FI6ZzmpKoUteyxftTwTZ34hMEPfQJ4EszkUC68ttThrc2Hmuh1DqrfGXFNZdpAw"
	// derived key_id for the vector seed's pubkey
	vectorKeyID = "dfccbffbf0f8b81d"
)

func vectorBody() *authv1.SessionTokenBody {
	return &authv1.SessionTokenBody{
		AccountId:      "acct-1",
		Handle:         "alice",
		TokenId:        "tok-1",
		Audience:       "cp",
		IssuedAt:       1700000000,
		ExpiresAt:      1700000900,
		SessionKeyHash: SessionKeyHash([]byte("vector-spki-bytes")),
		KeyId:          vectorKeyID,
	}
}

func TestGoldenVector(t *testing.T) {
	priv := ed25519.NewKeyFromSeed([]byte(vectorSeed))
	gotID, err := KeyID(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if gotID != vectorKeyID {
		t.Fatalf("vector key_id drift: got %s want %s", gotID, vectorKeyID)
	}
	wire, err := Mint(vectorBody(), priv)
	if err != nil {
		t.Fatal(err)
	}
	if wire != vectorWire {
		t.Fatalf("vector wire drift:\n got %s\nwant %s", wire, vectorWire)
	}
	ks, err := NewKeySet(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	body, err := Verify(vectorWire, ks, time.Unix(1700000010, 0))
	if err != nil {
		t.Fatalf("verify frozen vector: %v", err)
	}
	if !proto.Equal(body, vectorBody()) {
		t.Fatalf("vector body mismatch: %+v", body)
	}
}
