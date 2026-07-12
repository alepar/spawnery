package token

import (
	"bytes"
	"encoding/base64"
	"testing"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
)

func TestSignedAuthArtifactProtoRoundTrip(t *testing.T) {
	want := &authv1.SignedAuthArtifact{
		ArtifactType: "session-token",
		Payload:      []byte{0x00, 0x01, 0xfe, 0xff},
		Signature:    bytes.Repeat([]byte{0xa5}, 64),
		SignerChain: [][]byte{
			{0x30, 0x03, 0x01},
			{0x30, 0x03, 0x02},
		},
		KeyId: bytes.Repeat([]byte{0x5a}, 32),
	}

	encoded, err := proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	wire := base64.RawURLEncoding.EncodeToString(encoded)
	raw, err := base64.RawURLEncoding.DecodeString(wire)
	if err != nil {
		t.Fatal(err)
	}
	var got authv1.SignedAuthArtifact
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got.ArtifactType != want.ArtifactType ||
		!bytes.Equal(got.Payload, want.Payload) ||
		!bytes.Equal(got.Signature, want.Signature) ||
		!bytes.Equal(got.KeyId, want.KeyId) ||
		len(got.SignerChain) != len(want.SignerChain) {
		t.Fatalf("round trip mismatch: got %+v want %+v", &got, want)
	}
	for i := range want.SignerChain {
		if !bytes.Equal(got.SignerChain[i], want.SignerChain[i]) {
			t.Fatalf("chain[%d] mismatch: got %x want %x", i, got.SignerChain[i], want.SignerChain[i])
		}
	}
}
