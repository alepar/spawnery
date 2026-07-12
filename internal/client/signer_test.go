package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/intent"
)

func TestECDSASessionSignerUsesPersistentP256Key(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewECDSASessionSigner(key)
	if err != nil {
		t.Fatalf("NewECDSASessionSigner: %v", err)
	}

	wantSPKI, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	firstSPKI, err := signer.PublicSPKIDER()
	if err != nil {
		t.Fatalf("PublicSPKIDER: %v", err)
	}
	secondSPKI, err := signer.PublicSPKIDER()
	if err != nil {
		t.Fatalf("second PublicSPKIDER: %v", err)
	}
	if !reflect.DeepEqual(firstSPKI, wantSPKI) || !reflect.DeepEqual(secondSPKI, wantSPKI) {
		t.Fatal("PublicSPKIDER did not expose the persistent key")
	}

	body := []byte("exact protobuf bytes")
	domain := intent.DomainCreateSpawn
	sig, err := signer.SignP1363(domain, body)
	if err != nil {
		t.Fatalf("SignP1363: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature length = %d, want 64", len(sig))
	}
	if err := intent.VerifySig(domain, body, sig, firstSPKI); err != nil {
		t.Fatalf("VerifySig: %v", err)
	}
}

func TestECDSASessionSignerRejectsInvalidKeys(t *testing.T) {
	if _, err := NewECDSASessionSigner(nil); err == nil {
		t.Fatal("nil key accepted")
	}
	wrongCurve, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewECDSASessionSigner(wrongCurve); err == nil {
		t.Fatal("non-P256 key accepted")
	}
}

func TestNodeCredentialsCannotCarryCPToken(t *testing.T) {
	typ := reflect.TypeOf(NodeCredentials{})
	if _, ok := typ.FieldByName("CPAccessToken"); ok {
		t.Fatal("NodeCredentials exposes CPAccessToken")
	}
	if _, ok := typ.FieldByName("AccessToken"); !ok {
		t.Fatal("NodeCredentials lacks node access token")
	}
	var _ NodeCredentialSource = nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
		return NodeCredentials{}, nil
	})
}

func TestBuildSignedIntentSignsExactMarshaledBody(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewECDSASessionSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	body := &authv1.IntentBody{Op: string(intent.OpResumeSpawn), SpawnId: "sp-1", Generation: 7}
	wantBody, err := proto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	signed, err := buildSignedIntent(intent.OpResumeSpawn, body, signer)
	if err != nil {
		t.Fatalf("buildSignedIntent: %v", err)
	}
	if !reflect.DeepEqual(signed.Body, wantBody) {
		t.Fatal("signed body differs from the one protobuf marshal")
	}
	if signed.Domain != intent.DomainResumeSpawn {
		t.Fatalf("domain = %q", signed.Domain)
	}
	if err := intent.VerifySig(signed.Domain, signed.Body, signed.Sig, signed.SpkiDer); err != nil {
		t.Fatalf("VerifySig: %v", err)
	}
}

type nodeCredentialSourceFunc func(context.Context) (NodeCredentials, error)

func (f nodeCredentialSourceFunc) NodeCredentials(ctx context.Context) (NodeCredentials, error) {
	return f(ctx)
}
