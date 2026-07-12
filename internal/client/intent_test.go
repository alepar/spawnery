package client

// intent_test.go covers local tuple validation, target verification, and lifecycle signing.

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

// fakeIntentClient returns a ready PendingIntent immediately on GetPendingIntent.
// SubmitIntent always succeeds (we test that pollAndSign rejects before submitting).
type fakeIntentClient struct {
	pending   *cpv1.PendingIntent
	response  *cpv1.GetPendingIntentResponse
	submitted bool
	submit    *cpv1.SubmitIntentRequest
}

func (f *fakeIntentClient) GetPendingIntent(_ context.Context, _ *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error) {
	if f.response != nil {
		return connect.NewResponse(f.response), nil
	}
	return connect.NewResponse(&cpv1.GetPendingIntentResponse{Ready: true, Pending: f.pending}), nil
}

func (f *fakeIntentClient) SubmitIntent(_ context.Context, req *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error) {
	f.submitted = true
	f.submit = req.Msg
	return connect.NewResponse(&cpv1.SubmitIntentResponse{}), nil
}

func TestPollAndSignUsesVerifiedTargetPersistentSignerAndNodeToken(t *testing.T) {
	fx := issueProdNode(t, "node-1", "alice")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := NewECDSASessionSigner(key)
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
		return NodeCredentials{AccessToken: "node-token", Signer: signer}, nil
	})
	pending := &cpv1.PendingIntent{Op: string(intent.OpResumeSpawn), SpawnId: "sp-1", Generation: 7, TargetNodeId: "node-1"}
	ic := &fakeIntentClient{response: &cpv1.GetPendingIntentResponse{
		Ready: true, Pending: pending, Generation: 7, TargetNodeId: "node-1",
		TargetNodeClass: pki.ClassSelfHosted, TargetNodeAccountId: "alice", NodeCertChain: fx.chainPEM,
	}}
	trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: "dev.spawnery.internal", AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}

	if err := pollAndSign(context.Background(), ic, source, trust, "sp-1", IntentParams{Op: intent.OpResumeSpawn}); err != nil {
		t.Fatalf("pollAndSign: %v", err)
	}
	if ic.submit == nil || ic.submit.NodeAccessToken != "node-token" || ic.submit.Intent == nil {
		t.Fatalf("SubmitIntent = %+v", ic.submit)
	}
	wantSPKI, _ := signer.PublicSPKIDER()
	if string(ic.submit.Intent.SpkiDer) != string(wantSPKI) {
		t.Fatal("intent was not signed with the persistent key")
	}
}

func TestPollAndSignRejectsResponseTargetSubstitutionBeforeCredentials(t *testing.T) {
	called := false
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
		called = true
		return NodeCredentials{}, errors.New("must not be called")
	})
	pending := &cpv1.PendingIntent{Op: string(intent.OpResumeSpawn), SpawnId: "sp-1", Generation: 7, TargetNodeId: "node-1"}
	ic := &fakeIntentClient{response: &cpv1.GetPendingIntentResponse{Ready: true, Pending: pending, Generation: 7, TargetNodeId: "substituted"}}
	err := pollAndSign(context.Background(), ic, source, TargetTrust{}, "sp-1", IntentParams{Op: intent.OpResumeSpawn})
	if err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("error = %v", err)
	}
	if called || ic.submitted {
		t.Fatal("credentials or SubmitIntent used before tuple verification")
	}
}

func TestProvisionWithIntentReturnsSigningErrorPromptly(t *testing.T) {
	fx := issueProdNode(t, "node-1", "alice")
	pending := &cpv1.PendingIntent{Op: string(intent.OpResumeSpawn), SpawnId: "sp-1", Generation: 7, TargetNodeId: "node-1"}
	ic := &fakeIntentClient{response: &cpv1.GetPendingIntentResponse{Ready: true, Pending: pending, Generation: 7, TargetNodeId: "node-1", TargetNodeClass: pki.ClassSelfHosted, TargetNodeAccountId: "alice", NodeCertChain: fx.chainPEM}}
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
		return NodeCredentials{AccessToken: "node-token", Signer: failingSessionSigner{}}, nil
	})
	trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: "dev.spawnery.internal", AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}
	started := time.Now()
	err := provisionWithIntent(context.Background(), ic, source, trust, "sp-1", IntentParams{Op: intent.OpResumeSpawn}, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "sign failed") {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("signing error propagation took %s", elapsed)
	}
}

type failingSessionSigner struct{}

func (failingSessionSigner) PublicSPKIDER() ([]byte, error) {
	return nil, errors.New("public key failed")
}
func (failingSessionSigner) SignP1363(string, []byte) ([]byte, error) {
	return nil, errors.New("sign failed")
}

func TestPollAndSignRejectsUntrustedTargetBeforeCredentialsOrSubmit(t *testing.T) {
	fx := issueProdNode(t, "node-1", "alice")
	pending := &cpv1.PendingIntent{Op: string(intent.OpResumeSpawn), SpawnId: "sp-1", Generation: 7, TargetNodeId: "node-1"}
	base := &cpv1.GetPendingIntentResponse{
		Ready: true, Pending: pending, Generation: 7, TargetNodeId: "node-1", TargetNodeClass: pki.ClassSelfHosted,
		TargetNodeAccountId: "alice", NodeCertChain: fx.chainPEM,
	}
	for _, tc := range []struct {
		name   string
		mutate func(*cpv1.GetPendingIntentResponse, *TargetTrust)
	}{
		{name: "certificate id differs", mutate: func(r *cpv1.GetPendingIntentResponse, _ *TargetTrust) {
			r.TargetNodeId, r.Pending.TargetNodeId = "node-other", "node-other"
		}},
		{name: "foreign typed account", mutate: func(r *cpv1.GetPendingIntentResponse, _ *TargetTrust) { r.TargetNodeAccountId = "mallory" }},
		{name: "revoked certificate", mutate: func(_ *cpv1.GetPendingIntentResponse, trust *TargetTrust) {
			trust.CertificateRevocations = func(_, _ *big.Int) bool { return true }
		}},
		{name: "missing chain", mutate: func(r *cpv1.GetPendingIntentResponse, _ *TargetTrust) { r.NodeCertChain = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := proto.Clone(base).(*cpv1.GetPendingIntentResponse)
			trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: "dev.spawnery.internal", AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}
			tc.mutate(response, &trust)
			called := false
			source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) { called = true; return NodeCredentials{}, nil })
			ic := &fakeIntentClient{response: response}
			if err := pollAndSign(context.Background(), ic, source, trust, "sp-1", IntentParams{Op: intent.OpResumeSpawn}); err == nil {
				t.Fatal("untrusted target accepted")
			}
			if called || ic.submitted {
				t.Fatal("credentials or SubmitIntent used before target rejection")
			}
		})
	}
}

// TestAM1AppRefSubstitutionRejected: CP echoes a different app_ref than the user requested.
func TestAM1AppRefSubstitutionRejected(t *testing.T) {
	ic := &fakeIntentClient{pending: &cpv1.PendingIntent{
		Op:      "create-spawn",
		SpawnId: "sp-1",
		AppRef:  "evil-app/ref",
		Model:   "claude-3",
	}}
	err := pollAndSign(context.Background(), ic, nil, TargetTrust{}, "sp-1", IntentParams{Op: intent.OpCreateSpawn, AppRef: "myapp/ref", Model: "claude-3"})
	if err == nil {
		t.Fatal("expected AM1 rejection but got nil error")
	}
	if !strings.Contains(err.Error(), "AM1") || !strings.Contains(err.Error(), "app_ref") {
		t.Fatalf("expected AM1 app_ref error, got: %v", err)
	}
	if ic.submitted {
		t.Fatal("must not submit intent after AM1 rejection")
	}
}

// TestAM1ModelSubstitutionRejected: CP echoes a different model than the user requested.
func TestAM1ModelSubstitutionRejected(t *testing.T) {
	ic := &fakeIntentClient{pending: &cpv1.PendingIntent{
		Op:      "create-spawn",
		SpawnId: "sp-1",
		AppRef:  "myapp/ref",
		Model:   "gpt-5-malicious",
	}}
	err := pollAndSign(context.Background(), ic, nil, TargetTrust{}, "sp-1", IntentParams{Op: intent.OpCreateSpawn, AppRef: "myapp/ref", Model: "claude-3"})
	if err == nil {
		t.Fatal("expected AM1 rejection but got nil error")
	}
	if !strings.Contains(err.Error(), "AM1") || !strings.Contains(err.Error(), "model") {
		t.Fatalf("expected AM1 model error, got: %v", err)
	}
	if ic.submitted {
		t.Fatal("must not submit intent after AM1 rejection")
	}
}

// TestAM1TargetNodeSubstitutionRejected: CP echoes a different target_node_id than requested.
func TestAM1TargetNodeSubstitutionRejected(t *testing.T) {
	ic := &fakeIntentClient{pending: &cpv1.PendingIntent{
		Op:           "migrate-spawn",
		SpawnId:      "sp-1",
		TargetNodeId: "node-attacker",
	}}
	err := pollAndSign(context.Background(), ic, nil, TargetTrust{}, "sp-1", IntentParams{Op: intent.OpMigrateSpawn, TargetNodeID: "node-requested"})
	if err == nil {
		t.Fatal("expected AM1 rejection but got nil error")
	}
	if !strings.Contains(err.Error(), "AM1") || !strings.Contains(err.Error(), "target_node_id") {
		t.Fatalf("expected AM1 target_node_id error, got: %v", err)
	}
	if ic.submitted {
		t.Fatal("must not submit intent after AM1 rejection")
	}
}

// TestAM1CloudTargetSkipsNodeValidation: when TargetNodeID is empty (cloud placement),
// any target_node_id from the CP is accepted — the CP selects the actual node.
func TestAM1CloudTargetSkipsNodeValidation(t *testing.T) {
	ic := &fakeIntentClient{pending: &cpv1.PendingIntent{
		Op:           "migrate-spawn",
		SpawnId:      "sp-1",
		TargetNodeId: "node-cp-selected",
	}}
	// No TargetNodeID in params -> cloud placement, no validation.
	err := pollAndSign(context.Background(), ic, nil, TargetTrust{}, "sp-1", IntentParams{Op: intent.OpMigrateSpawn})
	// pollAndSign may still fail (e.g. crypto), but NOT with an AM1 error.
	if err != nil && strings.Contains(err.Error(), "AM1") {
		t.Fatalf("cloud target should not produce AM1 error, got: %v", err)
	}
}

// TestAM1MatchingParamsAccepted: when CP echoes exactly the requested params, no AM1 error.
func TestAM1MatchingParamsAccepted(t *testing.T) {
	ic := &fakeIntentClient{pending: &cpv1.PendingIntent{
		Op:      "create-spawn",
		SpawnId: "sp-1",
		AppRef:  "myapp/ref",
		Model:   "claude-3",
	}}
	err := pollAndSign(context.Background(), ic, nil, TargetTrust{}, "sp-1", IntentParams{Op: intent.OpCreateSpawn, AppRef: "myapp/ref", Model: "claude-3"})
	// No AM1 error; may fail for other reasons (build intent etc.) but not AM1.
	if err != nil && strings.Contains(err.Error(), "AM1") {
		t.Fatalf("matching params should not produce AM1 error, got: %v", err)
	}
}
