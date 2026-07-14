package token

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
)

// Frozen Go/TypeScript certificate-envelope vector. The root is local verifier configuration,
// while wire carries only the leaf-first signer chain.
const (
	vectorWire             = "Cg1zZXNzaW9uLXRva2VuEkgKBmFjY3QtMRIFYWxpY2UaBXRvay0xIgJjcCiA4s-qBjCE6c-qBjogOBp1BBDlDyEJCW8_HUeGeL2p98Yo_b70ULu6GR0jfeUaQAxrJCOn5ps9b8sINCEcvl64ljxbrADgVUqOxo8w3WjDL3o2uMuAPsVCQf_u6NPsl5Al4IAOVO3xqpqxRUNzRQAiwQMwggG9MIIBY6ADAgECAhB2ZWN0b3ItbGVhZi0wMDAxMAoGCCqGSM49BAMCMCwxKjAoBgNVBAMTIVNwYXduZXJ5IHZlY3RvciBhdXRoIGludGVybWVkaWF0ZTAeFw0yMzExMTQyMTEzMjBaFw0yNDAyMTIyMjEzMjBaMCoxKDAmBgNVBAMTH1NwYXduZXJ5IHZlY3RvciBhcnRpZmFjdCBzaWduZXIwKjAFBgMrZXADIQAjvFSRLB5uksSoaCXIZ-J__cVVv_vUJE8Xomq__-6WXaOBlzCBlDAOBgNVHQ8BAf8EBAMCB4AwHwYDVR0jBBgwFoAUqfMA61lg6JEzr3NiARoeJvDi6i4wSAYDVR0RBEEwP4Y9c3BpZmZlOi8vcHJvZC5zcGF3bmVyeS5pbnRlcm5hbC9zaWduZXIvYXV0aC1hcnRpZmFjdC92ZWN0b3ItMTAXBgNVHSAEEDAOMAwGCisGAQQBg78wAQIwCgYIKoZIzj0EAwIDSAAwRQIgO-FC6zHKCvMYrNBa5JWpzGUJEyc2gC8bDsA8jJ_F07ICIQCdw5wTrHs21vX4KlfppXXJ6nF4QN-lrgw2_SsletWuNCLKAzCCAcYwggFroAMCAQICEHZlY3Rvci1pbnRlci0wMDEwCgYIKoZIzj0EAwIwHzEdMBsGA1UEAxMUU3Bhd25lcnkgdmVjdG9yIHJvb3QwHhcNMjMxMTE0MTAxMzIwWhcNMjQwNTEyMjIxMzIwWjAsMSowKAYDVQQDEyFTcGF3bmVyeSB2ZWN0b3IgYXV0aCBpbnRlcm1lZGlhdGUwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAAR88nsYjQNPfopSOAMEtRrDwIlp4nfyGzWmC0j8R2aZeAd3VRDbjtBAKT2axp90MNu6fa3mPOmCKZ4Et50ieHPRo3wwejAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH_BAUwAwEB_zAdBgNVHQ4EFgQUqfMA61lg6JEzr3NiARoeJvDi6i4wHwYDVR0jBBgwFoAUaYvqY9xEo0RmP_FCmuoQhC3ye2swFwYDVR0gBBAwDjAMBgorBgEEAYO_MAEBMAoGCCqGSM49BAMCA0kAMEYCIQCI7Z9FaSO6JHUgS5fmaM-0tjLJERAri1BhMwEciWCGugIhANqvCJR2CK4nE8tIp1_4d0flrdDScI-fZ-VSi_VzJeLiKiDfzL_78Pi4HSXjxSI-b3QG-K0iI58kBBNmqdjytejjzg"
	vectorRootDER          = "MIIBgjCCASegAwIBAgIQdmVjdG9yLXJvb3QtMDAwMTAKBggqhkjOPQQDAjAfMR0wGwYDVQQDExRTcGF3bmVyeSB2ZWN0b3Igcm9vdDAeFw0yMzExMTMyMjEzMjBaFw0yNDExMTMyMjEzMjBaMB8xHTAbBgNVBAMTFFNwYXduZXJ5IHZlY3RvciByb290MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEaxfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpZP40Li_hp_m47n60p8D54WK84zV2sxXs7LtkBoN79R9aNFMEMwDgYDVR0PAQH_BAQDAgEGMBIGA1UdEwEB_wQIMAYBAf8CAQIwHQYDVR0OBBYEFGmL6mPcRKNEZj_xQprqEIQt8ntrMAoGCCqGSM49BAMCA0kAMEYCIQDHEEyVvHwyZrfK0q7-E0YJdwwLRzH1AL6td4AsyOrBdAIhANQyUFk2t93dETgV1rSadxmny3souI4T--x6Xqxh_RpY"
	vectorPayloadHex       = "0a06616363742d311205616c6963651a05746f6b2d31220263702880e2cfaa063084e9cfaa063a20381a750410e50f2109096f3f1d478678bda9f7c628fdbef450bbba191d237de5"
	vectorKeyIDHex         = "dfccbffbf0f8b81d25e3c5223e6f7406f8ad22239f24041366a9d8f2b5e8e3ce"
	vectorLeafDERHash      = "76caadf31f71eb8e0f6bd2fc4707bf59c9cff3a8361cab3dc0d4852c074ee226"
	vectorIntermediateHash = "74a5ecad98438a8302afe66781fa05ac8d4287106e1f9df69c42a680e84c7ad6"
)

func TestLegacyGoldenEnvelopeVectorRejected(t *testing.T) {
	raw, err := base64.RawURLEncoding.DecodeString(vectorWire)
	if err != nil {
		t.Fatal(err)
	}
	var envelope authv1.SignedAuthArtifact
	if err := proto.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(envelope.Payload); got != vectorPayloadHex {
		t.Fatalf("payload = %s, want %s", got, vectorPayloadHex)
	}
	if got := hex.EncodeToString(envelope.KeyId); got != vectorKeyIDHex {
		t.Fatalf("key ID = %s, want %s", got, vectorKeyIDHex)
	}
	if len(envelope.SignerChain) != 2 {
		t.Fatalf("chain length = %d, want 2", len(envelope.SignerChain))
	}
	if got := hashHex(envelope.SignerChain[0]); got != vectorLeafDERHash {
		t.Fatalf("leaf hash = %s, want %s", got, vectorLeafDERHash)
	}
	if got := hashHex(envelope.SignerChain[1]); got != vectorIntermediateHash {
		t.Fatalf("intermediate hash = %s, want %s", got, vectorIntermediateHash)
	}
	leaf, err := x509.ParseCertificate(envelope.SignerChain[0])
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := hashHex(spki); got != vectorKeyIDHex {
		t.Fatalf("SPKI key ID = %s, want %s", got, vectorKeyIDHex)
	}
	rootDER, err := base64.RawURLEncoding.DecodeString(vectorRootDER)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(vectorWire, ArtifactTypeSession, time.Unix(1_700_000_001, 0)); err == nil {
		t.Fatal("artifact carrying the retired policy OIDs was accepted")
	}
}

func hashHex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
