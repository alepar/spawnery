package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
)

// fakeMoveClient is a canned moveClient that records each request so runMove's orchestration can be
// asserted. node{Pub,Priv} model the target node's HPKE sub-key.
type fakeMoveClient struct {
	entries   []*cpv1.JournalKeyCiphertext
	nodeID    string
	nodePub   []byte
	gen       uint64
	notAfter  time.Time // STABLE sub-key expiry so the AAD reconstructs identically
	migErr    error
	getKeyErr error

	gotMigrate  *cpv1.MigrateSpawnRequest
	gotDelivery *cpv1.DeliverSecretsRequest
}

func (f *fakeMoveClient) GetJournalKeyCiphertext(_ context.Context, _ *connect.Request[cpv1.GetJournalKeyCiphertextRequest]) (*connect.Response[cpv1.GetJournalKeyCiphertextResponse], error) {
	return connect.NewResponse(&cpv1.GetJournalKeyCiphertextResponse{Entries: f.entries}), nil
}

func (f *fakeMoveClient) MigrateSpawn(_ context.Context, req *connect.Request[cpv1.MigrateSpawnRequest]) (*connect.Response[cpv1.MigrateSpawnResponse], error) {
	f.gotMigrate = req.Msg
	if f.migErr != nil {
		return nil, f.migErr
	}
	return connect.NewResponse(&cpv1.MigrateSpawnResponse{NodeId: f.nodeID}), nil
}

func (f *fakeMoveClient) GetSpawnNodeKey(_ context.Context, _ *connect.Request[cpv1.GetSpawnNodeKeyRequest]) (*connect.Response[cpv1.GetSpawnNodeKeyResponse], error) {
	if f.getKeyErr != nil {
		return nil, f.getKeyErr
	}
	sk := subkey.SignedSubKey{HPKEPub: f.nodePub, NodeID: f.nodeID, NotAfter: f.notAfter}
	skJSON, _ := json.Marshal(sk)
	return connect.NewResponse(&cpv1.GetSpawnNodeKeyResponse{SignedSubkey: skJSON, Generation: f.gen}), nil
}

func (f *fakeMoveClient) DeliverSecrets(_ context.Context, req *connect.Request[cpv1.DeliverSecretsRequest]) (*connect.Response[cpv1.DeliverSecretsResponse], error) {
	f.gotDelivery = req.Msg
	return connect.NewResponse(&cpv1.DeliverSecretsResponse{}), nil
}

func TestRunMoveResealsAndDelivers(t *testing.T) {
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
	if err := runMove(context.Background(), client, nil, dev, "sp1", "node-b", &out, time.Now()); err != nil {
		t.Fatalf("runMove: %v", err)
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
func TestRunMoveMigrateFailureIsDataSafe(t *testing.T) {
	mn, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(mn, "")
	client := &fakeMoveClient{
		entries: []*cpv1.JournalKeyCiphertext{{Mount: "main", Ciphertext: []byte("{}")}},
		migErr:  fmt.Errorf("no capacity"),
	}
	var out bytes.Buffer
	err := runMove(context.Background(), client, nil, dev, "sp1", "cloud", &out, time.Now())
	if err == nil || !strings.Contains(err.Error(), "your data is safe") {
		t.Fatalf("migrate failure err = %v, want a data-safe message", err)
	}
	if client.gotDelivery != nil {
		t.Fatal("must not deliver the journal key after a failed migrate")
	}
}
