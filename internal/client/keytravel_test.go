package client

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/intent"
	"spawnery/internal/pki"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
)

// fakeMoveClient is a canned moveClient that records each request so migrateSpawn's orchestration
// can be asserted. node{Pub,Priv} model the target node's HPKE sub-key. GetPendingIntent/
// SubmitIntent never report ready — migrateSpawn's concurrent pollAndSign goroutine just polls
// until MigrateSpawn returns and its context is cancelled; these tests assert the RPC orchestration,
// not the intent-signing (that is covered by intent_test.go).
type fakeMoveClient struct {
	entries        []*cpv1.JournalKeyCiphertext
	nodeID         string
	resolvedNodeID string
	nodePub        []byte
	nodeCertChain  []byte
	signedSubkey   []byte
	gen            uint64
	notAfter       time.Time // STABLE sub-key expiry so the AAD reconstructs identically
	migErr         error
	getKeyErr      error

	gotMigrate  *cpv1.MigrateSpawnRequest
	gotDelivery *cpv1.DeliverSecretsRequest
}

func (f *fakeMoveClient) GetPendingIntent(_ context.Context, _ *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error) {
	return connect.NewResponse(&cpv1.GetPendingIntentResponse{Ready: false}), nil
}

func (f *fakeMoveClient) SubmitIntent(_ context.Context, _ *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error) {
	return connect.NewResponse(&cpv1.SubmitIntentResponse{}), nil
}

func (f *fakeMoveClient) GetJournalKeyCiphertext(_ context.Context, _ *connect.Request[cpv1.GetJournalKeyCiphertextRequest]) (*connect.Response[cpv1.GetJournalKeyCiphertextResponse], error) {
	return connect.NewResponse(&cpv1.GetJournalKeyCiphertextResponse{Entries: f.entries}), nil
}

func (f *fakeMoveClient) MigrateSpawn(_ context.Context, req *connect.Request[cpv1.MigrateSpawnRequest]) (*connect.Response[cpv1.MigrateSpawnResponse], error) {
	f.gotMigrate = req.Msg
	if f.migErr != nil {
		return nil, f.migErr
	}
	nodeID := f.nodeID
	if f.resolvedNodeID != "" {
		nodeID = f.resolvedNodeID
	}
	return connect.NewResponse(&cpv1.MigrateSpawnResponse{NodeId: nodeID, TransferSetId: "ts-test"}), nil
}

func (f *fakeMoveClient) GetSpawnNodeKey(_ context.Context, _ *connect.Request[cpv1.GetSpawnNodeKeyRequest]) (*connect.Response[cpv1.GetSpawnNodeKeyResponse], error) {
	if f.getKeyErr != nil {
		return nil, f.getKeyErr
	}
	sk := subkey.SignedSubKey{HPKEPub: f.nodePub, NodeID: f.nodeID, NotAfter: f.notAfter}
	skJSON, _ := json.Marshal(sk)
	if len(f.signedSubkey) != 0 {
		skJSON = f.signedSubkey
	}
	return connect.NewResponse(&cpv1.GetSpawnNodeKeyResponse{
		NodeCertChain: f.nodeCertChain,
		SignedSubkey:  skJSON,
		Generation:    f.gen,
	}), nil
}

func (f *fakeMoveClient) DeliverSecrets(_ context.Context, req *connect.Request[cpv1.DeliverSecretsRequest]) (*connect.Response[cpv1.DeliverSecretsResponse], error) {
	f.gotDelivery = req.Msg
	return connect.NewResponse(&cpv1.DeliverSecretsResponse{}), nil
}

func TestMigrateResealsAndDelivers(t *testing.T) {
	// Owner device + an owner-sealed journal-key envelope for mount "main".
	mn, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(mn, "")
	const password = "repo-pw-123"
	env, err := journalkey.SealToOwner(password, []seal.X25519PubKey{dev.X25519PubKey()},
		seal.AtRestAAD{AccountID: "alice", SecretID: journalkey.SecretID("main"), Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := json.Marshal(env)

	// Target node B's sub-keypair.
	bPub, bPriv, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(time.Hour).Round(0)
	client := &fakeMoveClient{
		entries: []*cpv1.JournalKeyCiphertext{{Mount: "main", Ciphertext: ct}},
		nodeID:  "node-b", nodePub: bPub, gen: 7, notAfter: notAfter,
	}

	// Pin the delivery nonce so the test can reconstruct the in-flight AAD and open on node B.
	prev := genDeliveryID
	genDeliveryID = func() string { return "fixed-delivery" }
	defer func() { genDeliveryID = prev }()

	var out bytes.Buffer
	if err := migrateSpawn(context.Background(), client, dev, "sp1", "node-b", &out, time.Now(), MoveOptions{}, nil); err != nil {
		t.Fatalf("migrateSpawn: %v", err)
	}

	// MigrateSpawn was driven to the right node target.
	if client.gotMigrate == nil || client.gotMigrate.TargetNodeId != "node-b" || client.gotMigrate.TargetClass != "" {
		t.Fatalf("MigrateSpawn req = %+v, want TargetNodeId=node-b", client.gotMigrate)
	}
	// DeliverSecrets carried the journal-key secret id for the mount.
	if client.gotDelivery == nil || len(client.gotDelivery.Secrets) != 1 {
		t.Fatalf("DeliverSecrets req = %+v, want 1 secret", client.gotDelivery)
	}
	sec := client.gotDelivery.Secrets[0]
	if sec.SecretId != journalkey.SecretID("main") {
		t.Fatalf("delivered secret id = %q, want %q", sec.SecretId, journalkey.SecretID("main"))
	}
	if sec.Version != 7 || sec.DeliveryId != "fixed-delivery" {
		t.Fatalf("delivery metadata = version %d id %q, want version 7 id fixed-delivery", sec.Version, sec.DeliveryId)
	}
	// Node B opens the delivered ciphertext (proving the reseal targeted B's key + AAD).
	var sealed seal.NodeSealed
	if err := json.Unmarshal(sec.Sealed, &sealed); err != nil {
		t.Fatal(err)
	}
	aad := seal.InFlightAAD{
		SpawnID: "sp1", Generation: 7, NodeID: "node-b",
		NotAfter: notAfter, Version: 7, DeliveryID: "fixed-delivery",
	}
	recovered, err := seal.OpenFromOwner(&sealed, bPriv, aad, time.Now())
	if err != nil {
		t.Fatalf("node B OpenFromOwner: %v", err)
	}
	if string(recovered) != password {
		t.Fatalf("node B recovered %q, want %q", recovered, password)
	}
	if !strings.Contains(out.String(), "journal key delivered") {
		t.Fatalf("progress output missing delivery line:\n%s", out.String())
	}
}

// A class target maps onto MigrateSpawnRequest.TargetClass.
func TestMigrateTargetMapping(t *testing.T) {
	if r := migrateTarget("sp1", "cloud"); r.TargetClass != "cloud" || r.TargetNodeId != "" {
		t.Fatalf("cloud target = %+v", r)
	}
	if r := migrateTarget("sp1", "node-9"); r.TargetNodeId != "node-9" || r.TargetClass != "" {
		t.Fatalf("node target = %+v", r)
	}
}

// A MigrateSpawn failure is reported with a data-safe message and never proceeds to deliver the key.
func TestMigrateFailureIsDataSafe(t *testing.T) {
	mn, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(mn, "")
	client := &fakeMoveClient{
		entries: []*cpv1.JournalKeyCiphertext{{Mount: "main", Ciphertext: []byte("{}")}},
		migErr:  fmt.Errorf("no capacity"),
	}
	var out bytes.Buffer
	err := migrateSpawn(context.Background(), client, dev, "sp1", "cloud", &out, time.Now(), MoveOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "your data is safe") {
		t.Fatalf("migrate failure err = %v, want a data-safe message", err)
	}
	if client.gotDelivery != nil {
		t.Fatal("must not deliver the journal key after a failed migrate")
	}
}

type prodNodeFix struct {
	rootPEM  []byte
	chainPEM []byte
	key      *ecdsa.PrivateKey
	nodeID   string
}

func issueProdNode(t *testing.T, nodeID, accountID string) prodNodeFix {
	return issueProdNodeClass(t, nodeID, accountID, pki.ClassSelfHosted)
}

func issueProdNodeClass(t *testing.T, nodeID, accountID string, class pki.IssuerRole) prodNodeFix {
	t.Helper()

	root, err := pki.NewRootCA("test-root")
	if err != nil {
		t.Fatal(err)
	}
	inter, err := root.NewIntermediate(class)
	if err != nil {
		t.Fatal(err)
	}
	node, err := inter.IssueNode(nodeID, accountID, string(class), time.Now().Add(365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return prodNodeFix{
		rootPEM:  pki.MarshalCertPEM(root.Cert),
		chainPEM: append(pki.MarshalCertPEM(node.Cert), pki.MarshalCertPEM(inter.Cert)...),
		key:      node.Key,
		nodeID:   nodeID,
	}
}

func TestMigrateRejectsRevokedCertificateBeforeDelivery(t *testing.T) {
	mn, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(mn, "")
	env, err := journalkey.SealToOwner("repo-pw-123", []seal.X25519PubKey{dev.X25519PubKey()},
		seal.AtRestAAD{AccountID: "alice", SecretID: journalkey.SecretID("main"), Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := json.Marshal(env)

	fx := issueProdNode(t, "node-b", "alice")
	nodePub, _, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sk, err := subkey.Sign(fx.key, fx.nodeID, nodePub, now, now.Add(subkey.DefaultValidity))
	if err != nil {
		t.Fatal(err)
	}

	client := &fakeMoveClient{
		entries:       []*cpv1.JournalKeyCiphertext{{Mount: "main", Ciphertext: ct}},
		nodeID:        fx.nodeID,
		nodePub:       sk.HPKEPub,
		nodeCertChain: fx.chainPEM,
		gen:           7,
		notAfter:      sk.NotAfter,
	}

	var out bytes.Buffer
	err = migrateSpawn(context.Background(), client, dev, "sp1", fx.nodeID, &out, now, MoveOptions{
		AccountID:              "alice",
		RootPEM:                fx.rootPEM,
		TrustDomain:            pki.DefaultTrustDomain,
		CertificateRevocations: func(_, _ *big.Int) bool { return true },
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "certificate is revoked") {
		t.Fatalf("err = %v", err)
	}
	if client.gotDelivery != nil {
		t.Fatal("DeliverSecrets must not be called for a revoked node")
	}
}

func TestMigrateRejectsMismatchedVerifiedNodeBeforeDelivery(t *testing.T) {
	mn, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(mn, "")
	env, err := journalkey.SealToOwner("repo-pw-123", []seal.X25519PubKey{dev.X25519PubKey()},
		seal.AtRestAAD{AccountID: "alice", SecretID: journalkey.SecretID("main"), Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := json.Marshal(env)

	fx := issueProdNode(t, "node-c", "alice")
	nodePub, _, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sk, err := subkey.Sign(fx.key, fx.nodeID, nodePub, now, now.Add(subkey.DefaultValidity))
	if err != nil {
		t.Fatal(err)
	}
	skJSON, _ := json.Marshal(sk)

	client := &fakeMoveClient{
		entries:        []*cpv1.JournalKeyCiphertext{{Mount: "main", Ciphertext: ct}},
		nodeID:         fx.nodeID,
		resolvedNodeID: "node-b",
		nodePub:        sk.HPKEPub,
		nodeCertChain:  fx.chainPEM,
		signedSubkey:   skJSON,
		gen:            7,
		notAfter:       sk.NotAfter,
	}

	var out bytes.Buffer
	err = migrateSpawn(context.Background(), client, dev, "sp1", "node-b", &out, now, MoveOptions{
		AccountID:              "alice",
		RootPEM:                fx.rootPEM,
		TrustDomain:            pki.DefaultTrustDomain,
		CertificateRevocations: allowNoCertificateRevocations,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "verified node \"node-c\" does not match resolved node \"node-b\"") {
		t.Fatalf("err = %v", err)
	}
	if client.gotDelivery != nil {
		t.Fatal("DeliverSecrets must not be called when verified node differs from resolved node")
	}
}

func TestMigrateRejectsMissingCertChainWhenRootConfigured(t *testing.T) {
	mn, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(mn, "")
	env, err := journalkey.SealToOwner("repo-pw-123", []seal.X25519PubKey{dev.X25519PubKey()},
		seal.AtRestAAD{AccountID: "alice", SecretID: journalkey.SecretID("main"), Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := json.Marshal(env)

	nodePub, _, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeMoveClient{
		entries:  []*cpv1.JournalKeyCiphertext{{Mount: "main", Ciphertext: ct}},
		nodeID:   "node-b",
		nodePub:  nodePub,
		gen:      7,
		notAfter: time.Now().Add(time.Hour).Round(0),
	}

	var out bytes.Buffer
	err = migrateSpawn(context.Background(), client, dev, "sp1", "node-b", &out, time.Now(), MoveOptions{
		RootPEM: []byte("pinned-root-pem"), TrustDomain: pki.DefaultTrustDomain,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "node cert chain") {
		t.Fatalf("err = %v", err)
	}
	if client.gotDelivery != nil {
		t.Fatal("DeliverSecrets must not be called when a pinned root is configured but CP omits the node cert chain")
	}
}

// ---- Fork ---------------------------------------------------------------------------

type fakeForkClient struct {
	entries    []*cpv1.JournalKeyCiphertext
	nodeID     string
	nodePub    []byte
	gen        uint64
	notAfter   time.Time
	forkID     string
	transferID string
	forkErr    error
	forkDelay  time.Duration
	deliverErr error

	gotFork              *cpv1.ForkSpawnRequest
	gotIntentPollSpawnID string
	gotJournalSpawnID    string
	gotNodeKeySpawnID    string
	gotDelivery          *cpv1.DeliverSecretsRequest
}

type readyForkClient struct {
	*fakeForkClient
	pending *cpv1.GetPendingIntentResponse
}

func (f *readyForkClient) GetPendingIntent(_ context.Context, req *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error) {
	f.gotIntentPollSpawnID = req.Msg.SpawnId
	return connect.NewResponse(f.pending), nil
}

func TestForkWaitsForLateAuthorizationFailureAfterRPCSuccess(t *testing.T) {
	fx := issueProdNode(t, "node-a", "alice")
	base := &fakeForkClient{forkID: "fork-1", nodeID: "node-a"}
	client := &readyForkClient{fakeForkClient: base, pending: &cpv1.GetPendingIntentResponse{
		Ready: true, Generation: 7, TargetNodeId: "node-a", TargetNodeClass: pki.ClassSelfHosted,
		TargetNodeAccountId: "alice", NodeCertChain: fx.chainPEM,
		Pending: &cpv1.PendingIntent{Op: string(intent.OpForkSpawn), SpawnId: "source-1", Generation: 7, TargetNodeId: "node-a"},
	}}
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
		time.Sleep(25 * time.Millisecond)
		return NodeCredentials{}, errors.New("late fork authorization failure")
	})
	trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: pki.DefaultTrustDomain, AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}
	_, err := forkSpawnAuthorized(context.Background(), client, source, trust, testForkDevice(t), &cpv1.ForkSpawnRequest{SpawnId: "source-1"}, io.Discard, time.Now(), MoveOptions{})
	if err == nil || !strings.Contains(err.Error(), "late fork authorization failure") {
		t.Fatalf("error = %v", err)
	}
	if base.gotJournalSpawnID != "" {
		t.Fatal("fork continued to journal delivery before authorization completed")
	}
}

func TestMoveAndForkPreflightFailureMakesNoCPCall(t *testing.T) {
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) { return NodeCredentials{}, errors.New("login required") })
	move := &fakeMoveClient{}
	if err := migrateSpawnAuthorized(context.Background(), move, source, TargetTrust{}, testForkDevice(t), "sp-1", "cloud", io.Discard, time.Now(), MoveOptions{}, nil, true); err == nil {
		t.Fatal("move accepted missing credentials")
	}
	if move.gotMigrate != nil {
		t.Fatal("MigrateSpawn called before credential preflight")
	}
	fork := &fakeForkClient{}
	if _, err := forkSpawnAuthorized(context.Background(), fork, source, TargetTrust{}, testForkDevice(t), &cpv1.ForkSpawnRequest{SpawnId: "sp-1"}, io.Discard, time.Now(), MoveOptions{}); err == nil {
		t.Fatal("fork accepted missing credentials")
	}
	if fork.gotFork != nil {
		t.Fatal("ForkSpawn called before credential preflight")
	}
}

func (f *fakeForkClient) ForkSpawn(_ context.Context, req *connect.Request[cpv1.ForkSpawnRequest]) (*connect.Response[cpv1.ForkSpawnResponse], error) {
	f.gotFork = req.Msg
	time.Sleep(f.forkDelay)
	if f.forkErr != nil {
		return nil, f.forkErr
	}
	return connect.NewResponse(&cpv1.ForkSpawnResponse{
		ForkSpawnId:   f.forkID,
		NodeId:        f.nodeID,
		TransferSetId: f.transferID,
	}), nil
}

type signOnlyFailer struct{}

func (signOnlyFailer) PublicSPKIDER() ([]byte, error) { return []byte{1}, nil }
func (signOnlyFailer) SignP1363(string, []byte) ([]byte, error) {
	return nil, errors.New("fork sign failed")
}

func TestForkAuthorizationErrorCancelsAndDrainsDelayedRPCPeer(t *testing.T) {
	fx := issueProdNode(t, "node-a", "alice")
	base := &fakeForkClient{forkID: "fork-1", nodeID: "node-a", forkDelay: 30 * time.Millisecond}
	client := &readyForkClient{fakeForkClient: base, pending: &cpv1.GetPendingIntentResponse{
		Ready: true, Generation: 7, TargetNodeId: "node-a", TargetNodeClass: pki.ClassSelfHosted,
		TargetNodeAccountId: "alice", NodeCertChain: fx.chainPEM,
		Pending: &cpv1.PendingIntent{Op: string(intent.OpForkSpawn), SpawnId: "source-1", Generation: 7, TargetNodeId: "node-a"},
	}}
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) {
		return NodeCredentials{AccessToken: "node-token", Signer: signOnlyFailer{}}, nil
	})
	trust := TargetTrust{RootPEM: fx.rootPEM, TrustDomain: pki.DefaultTrustDomain, AccountID: "alice", CertificateRevocations: func(_, _ *big.Int) bool { return false }, Now: time.Now}
	started := time.Now()
	_, err := forkSpawnAuthorized(context.Background(), client, source, trust, testForkDevice(t), &cpv1.ForkSpawnRequest{SpawnId: "source-1"}, io.Discard, time.Now(), MoveOptions{})
	if err == nil || !strings.Contains(err.Error(), "fork sign failed") {
		t.Fatalf("error = %v", err)
	}
	if time.Since(started) < 25*time.Millisecond {
		t.Fatal("fork returned before delayed RPC peer drained")
	}
}

func (f *fakeForkClient) GetPendingIntent(_ context.Context, req *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error) {
	f.gotIntentPollSpawnID = req.Msg.SpawnId
	// Never report ready: the concurrent pollAndSign goroutine keeps polling until ForkSpawn
	// returns and its context is cancelled. Tests only assert the source id is the poll key.
	return connect.NewResponse(&cpv1.GetPendingIntentResponse{Ready: false}), nil
}

func (f *fakeForkClient) SubmitIntent(_ context.Context, _ *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error) {
	return connect.NewResponse(&cpv1.SubmitIntentResponse{}), nil
}

func (f *fakeForkClient) GetJournalKeyCiphertext(_ context.Context, req *connect.Request[cpv1.GetJournalKeyCiphertextRequest]) (*connect.Response[cpv1.GetJournalKeyCiphertextResponse], error) {
	f.gotJournalSpawnID = req.Msg.SpawnId
	return connect.NewResponse(&cpv1.GetJournalKeyCiphertextResponse{Entries: f.entries}), nil
}

func (f *fakeForkClient) GetSpawnNodeKey(_ context.Context, req *connect.Request[cpv1.GetSpawnNodeKeyRequest]) (*connect.Response[cpv1.GetSpawnNodeKeyResponse], error) {
	f.gotNodeKeySpawnID = req.Msg.SpawnId
	sk := subkey.SignedSubKey{HPKEPub: f.nodePub, NodeID: f.nodeID, NotAfter: f.notAfter}
	skJSON, _ := json.Marshal(sk)
	return connect.NewResponse(&cpv1.GetSpawnNodeKeyResponse{SignedSubkey: skJSON, Generation: f.gen}), nil
}

func (f *fakeForkClient) DeliverSecrets(_ context.Context, req *connect.Request[cpv1.DeliverSecretsRequest]) (*connect.Response[cpv1.DeliverSecretsResponse], error) {
	f.gotDelivery = req.Msg
	if f.deliverErr != nil {
		return nil, f.deliverErr
	}
	return connect.NewResponse(&cpv1.DeliverSecretsResponse{}), nil
}

func TestForkCallsForkSpawnDefaultSameNode(t *testing.T) {
	dev := testForkDevice(t)
	client := &fakeForkClient{forkID: "fork-1", nodeID: "node-a", transferID: "ts-1"}
	var out bytes.Buffer

	result, err := forkSpawn(context.Background(), client, dev, &cpv1.ForkSpawnRequest{SpawnId: "source-1"}, &out, time.Now(), MoveOptions{}, nil)
	if err != nil {
		t.Fatalf("forkSpawn: %v", err)
	}

	if client.gotFork == nil || client.gotFork.SpawnId != "source-1" || client.gotFork.TargetNodeId != "" || client.gotFork.TargetClass != "" {
		t.Fatalf("ForkSpawn req = %+v", client.gotFork)
	}
	if client.gotJournalSpawnID != "fork-1" {
		t.Fatalf("journal ciphertext requested for %q, want fork-1", client.gotJournalSpawnID)
	}
	if result.ForkSpawnID != "fork-1" || result.NodeID != "node-a" || result.TransferSetID != "ts-1" {
		t.Fatalf("ForkResult = %+v", result)
	}
	if !strings.Contains(out.String(), "fork fork-1 active on node node-a") {
		t.Fatalf("output missing fork summary:\n%s", out.String())
	}
}

func TestForkPassesNameAndNode(t *testing.T) {
	dev := testForkDevice(t)
	client := &fakeForkClient{forkID: "fork-2", nodeID: "node-b"}
	var out bytes.Buffer

	req := &cpv1.ForkSpawnRequest{SpawnId: "source-1", TargetNodeId: "node-b", Name: "Trial"}
	if _, err := forkSpawn(context.Background(), client, dev, req, &out, time.Now(), MoveOptions{}, nil); err != nil {
		t.Fatalf("forkSpawn: %v", err)
	}

	if client.gotFork == nil || client.gotFork.TargetNodeId != "node-b" || client.gotFork.TargetClass != "" || client.gotFork.Name != "Trial" {
		t.Fatalf("ForkSpawn req = %+v, want node-b + Trial", client.gotFork)
	}
}

func TestForkOwnerSealedDeliveryUsesForkID(t *testing.T) {
	dev, entry := testForkOwnerSealedEntry(t)
	nodePub, _, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeForkClient{
		entries: []*cpv1.JournalKeyCiphertext{entry},
		forkID:  "fork-3", nodeID: "node-b", nodePub: nodePub, gen: 9, notAfter: time.Now().Add(time.Hour).Round(0),
	}

	prev := genDeliveryID
	genDeliveryID = func() string { return "fixed-fork-delivery" }
	defer func() { genDeliveryID = prev }()

	var out bytes.Buffer
	req := &cpv1.ForkSpawnRequest{SpawnId: "source-1", TargetNodeId: "node-b"}
	result, err := forkSpawn(context.Background(), client, dev, req, &out, time.Now(), MoveOptions{}, nil)
	if err != nil {
		t.Fatalf("forkSpawn: %v", err)
	}

	if client.gotJournalSpawnID != "fork-3" {
		t.Fatalf("journal key lookup used %q, want fork-3", client.gotJournalSpawnID)
	}
	if client.gotNodeKeySpawnID != "fork-3" {
		t.Fatalf("node key lookup used %q, want fork-3", client.gotNodeKeySpawnID)
	}
	if client.gotDelivery == nil || client.gotDelivery.SpawnId != "fork-3" {
		t.Fatalf("DeliverSecrets req = %+v, want fork-3", client.gotDelivery)
	}
	if result.Delivered != 1 {
		t.Fatalf("ForkResult.Delivered = %d, want 1", result.Delivered)
	}
}

func TestForkDeliveryFailureReportsPending(t *testing.T) {
	dev, entry := testForkOwnerSealedEntry(t)
	nodePub, _, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeForkClient{
		entries:    []*cpv1.JournalKeyCiphertext{entry},
		forkID:     "fork-4",
		nodeID:     "node-b",
		nodePub:    nodePub,
		gen:        3,
		notAfter:   time.Now().Add(time.Hour).Round(0),
		deliverErr: fmt.Errorf("node offline"),
	}

	var out bytes.Buffer
	req := &cpv1.ForkSpawnRequest{SpawnId: "source-1", TargetNodeId: "node-b"}
	_, err = forkSpawn(context.Background(), client, dev, req, &out, time.Now(), MoveOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "fork created as fork-4") || !strings.Contains(err.Error(), "delivery pending") {
		t.Fatalf("delivery failure err = %v, want pending fork copy", err)
	}
}

func testForkDevice(t *testing.T) *seal.Device {
	t.Helper()
	mn, err := seal.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := seal.DeviceFromMnemonic(mn, "")
	if err != nil {
		t.Fatal(err)
	}
	return dev
}

func testForkOwnerSealedEntry(t *testing.T) (*seal.Device, *cpv1.JournalKeyCiphertext) {
	t.Helper()
	dev := testForkDevice(t)
	env, err := journalkey.SealToOwner("repo-pw-123", []seal.X25519PubKey{dev.X25519PubKey()},
		seal.AtRestAAD{AccountID: "alice", SecretID: journalkey.SecretID("main"), Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	ct, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return dev, &cpv1.JournalKeyCiphertext{Mount: "main", Ciphertext: ct}
}
