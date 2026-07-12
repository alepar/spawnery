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
	"google.golang.org/protobuf/proto"

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
	fx := issueProdNode(t, "node-1", "alice")
	source, trust := testSessionAuthorization(t, fx)
	rpc := &fakeSessionTargetClient{response: &cpv1.GetSpawnNodeKeyResponse{Generation: 6}}
	_, err := buildSessionOpenIntent(context.Background(), rpc, source, trust, "sp-1", 7, "0")
	if err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildSessionOpenIntentPropagatesRPCAndSignerErrors(t *testing.T) {
	t.Run("CP lookup", func(t *testing.T) {
		fx := issueProdNode(t, "node-1", "alice")
		source, trust := testSessionAuthorization(t, fx)
		rpc := &fakeSessionTargetClient{err: errors.New("lookup failed")}
		_, err := buildSessionOpenIntent(context.Background(), rpc, source, trust, "sp-1", 7, "0")
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
			return NodeCredentials{AccessToken: "node-token", Signer: signOnlyFailer{}}, nil
		})
		trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: "dev.spawnery.internal", AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}
		_, err := buildSessionOpenIntent(context.Background(), rpc, source, trust, "sp-1", 7, "0")
		if err == nil || !strings.Contains(err.Error(), "fork sign failed") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestBuildSessionOpenIntentRejectsTargetAndTrustSubstitutionBeforeCredentials(t *testing.T) {
	fx := issueProdNode(t, "node-1", "alice")
	base := &cpv1.GetSpawnNodeKeyResponse{
		Generation: 7, TargetNodeId: "node-1", TargetNodeClass: pki.ClassSelfHosted,
		TargetNodeAccountId: "alice", NodeCertChain: fx.chainPEM,
	}
	for _, tc := range []struct {
		name              string
		expectCredentials bool
		mutate            func(*cpv1.GetSpawnNodeKeyResponse, *TargetTrust)
	}{
		{name: "node id", expectCredentials: true, mutate: func(r *cpv1.GetSpawnNodeKeyResponse, _ *TargetTrust) { r.TargetNodeId = "other" }},
		{name: "account", expectCredentials: true, mutate: func(r *cpv1.GetSpawnNodeKeyResponse, _ *TargetTrust) { r.TargetNodeAccountId = "mallory" }},
		{name: "class", expectCredentials: true, mutate: func(r *cpv1.GetSpawnNodeKeyResponse, trust *TargetTrust) {
			r.TargetNodeClass, r.TargetNodeAccountId, trust.CloudAccountID = pki.ClassCloud, "spawnery-system", "spawnery-system"
		}},
		{name: "missing trust", mutate: func(_ *cpv1.GetSpawnNodeKeyResponse, trust *TargetTrust) { *trust = TargetTrust{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := proto.Clone(base).(*cpv1.GetSpawnNodeKeyResponse)
			trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: pki.DefaultTrustDomain, AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}
			tc.mutate(response, &trust)
			called := false
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			baseSigner, _ := NewECDSASessionSigner(key)
			signer := &trackingSessionSigner{SessionSigner: baseSigner}
			source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
				called = true
				return NodeCredentials{AccessToken: "node-token", Signer: signer}, nil
			})
			if _, err := buildSessionOpenIntent(context.Background(), &fakeSessionTargetClient{response: response}, source, trust, "sp-1", 7, "0"); err == nil {
				t.Fatal("substituted target accepted")
			}
			if called != tc.expectCredentials || signer.signs != 0 {
				t.Fatalf("credential preflight=%v signs=%d", called, signer.signs)
			}
		})
	}
}

type trackingSessionSigner struct {
	SessionSigner
	signs int
}

func (s *trackingSessionSigner) SignP1363(domain string, body []byte) ([]byte, error) {
	s.signs++
	return s.SessionSigner.SignP1363(domain, body)
}

func testSessionAuthorization(t *testing.T, fx prodNodeFix) (NodeCredentialSource, TargetTrust) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer, _ := NewECDSASessionSigner(key)
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
		return NodeCredentials{AccessToken: "node-token", Signer: signer}, nil
	})
	trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: pki.DefaultTrustDomain, AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}
	return source, trust
}

func TestBuildSessionOpenIntentRejectsMissingCredentials(t *testing.T) {
	fx := issueProdNode(t, "node-1", "alice")
	rpc := &fakeSessionTargetClient{response: &cpv1.GetSpawnNodeKeyResponse{Generation: 7, TargetNodeId: "node-1", TargetNodeClass: pki.ClassSelfHosted, TargetNodeAccountId: "alice", NodeCertChain: fx.chainPEM}}
	trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: pki.DefaultTrustDomain, AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}
	if _, err := buildSessionOpenIntent(context.Background(), rpc, nil, trust, "sp-1", 7, "0"); err == nil {
		t.Fatal("session open accepted missing credentials")
	}
	if rpc.calls != 0 {
		t.Fatalf("GetSpawnNodeKey called %d times before credential preflight", rpc.calls)
	}
}
