package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/intent"
	"spawnery/internal/pki"
)

type fakeSessionTargetClient struct {
	response *cpv1.GetSpawnNodeKeyResponse
	err      error
	calls    int
}

func (f *fakeSessionTargetClient) GetSpawnNodeKey(_ context.Context, _ *connect.Request[cpv1.GetSpawnNodeKeyRequest]) (*connect.Response[cpv1.GetSpawnNodeKeyResponse], error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.response), nil
}

func TestBuildSessionOpenIntentUsesResolvedTargetNodeTokenAndPersistentKey(t *testing.T) {
	fx := issueProdNode(t, "node-1", "alice")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer, _ := NewECDSASessionSigner(key)
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
		return NodeCredentials{AccessToken: "node-token", Signer: signer}, nil
	})
	rpc := &fakeSessionTargetClient{response: &cpv1.GetSpawnNodeKeyResponse{
		Generation: 7, TargetNodeId: "node-1", TargetNodeClass: pki.ClassSelfHosted,
		TargetNodeAccountId: "alice", NodeCertChain: fx.chainPEM,
	}}
	trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: "dev.spawnery.internal", AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}

	env, err := buildSessionOpenIntent(context.Background(), rpc, source, trust, "sp-1", 7, "0")
	if err != nil {
		t.Fatalf("buildSessionOpenIntent: %v", err)
	}
	if env.AccessToken != "node-token" {
		t.Fatalf("access token = %q", env.AccessToken)
	}
	body, err := intent.ParseBody(env.Intent.Body)
	if err != nil {
		t.Fatal(err)
	}
	if body.Op != string(intent.OpSessionOpen) || body.SpawnId != "sp-1" || body.Generation != 7 || body.TargetNodeId != "node-1" || body.SessionId != "0" {
		t.Fatalf("body = %+v", body)
	}
	wantSPKI, _ := signer.PublicSPKIDER()
	if string(env.Intent.SpkiDer) != string(wantSPKI) {
		t.Fatal("session intent did not use persistent key")
	}
}

func TestBuildSessionOpenIntentRejectsStaleGenerationBeforeCredentials(t *testing.T) {
	called := false
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
		called = true
		return NodeCredentials{}, nil
	})
	rpc := &fakeSessionTargetClient{response: &cpv1.GetSpawnNodeKeyResponse{Generation: 6}}
	_, err := buildSessionOpenIntent(context.Background(), rpc, source, TargetTrust{}, "sp-1", 7, "0")
	if err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("credentials loaded before generation validation")
	}
}

func TestBuildSessionOpenIntentPropagatesRPCAndSignerErrors(t *testing.T) {
	t.Run("CP lookup", func(t *testing.T) {
		rpc := &fakeSessionTargetClient{err: errors.New("lookup failed")}
		_, err := buildSessionOpenIntent(context.Background(), rpc, nil, TargetTrust{}, "sp-1", 7, "0")
		if err == nil || !strings.Contains(err.Error(), "lookup failed") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("signer", func(t *testing.T) {
		fx := issueProdNode(t, "node-1", "alice")
		rpc := &fakeSessionTargetClient{response: &cpv1.GetSpawnNodeKeyResponse{
			Generation: 7, TargetNodeId: "node-1", TargetNodeClass: pki.ClassSelfHosted,
			TargetNodeAccountId: "alice", NodeCertChain: fx.chainPEM,
		}}
		source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
			return NodeCredentials{AccessToken: "node-token", Signer: failingSessionSigner{}}, nil
		})
		trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: "dev.spawnery.internal", AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}
		_, err := buildSessionOpenIntent(context.Background(), rpc, source, trust, "sp-1", 7, "0")
		if err == nil || !strings.Contains(err.Error(), "sign failed") {
			t.Fatalf("error = %v", err)
		}
	})
}
