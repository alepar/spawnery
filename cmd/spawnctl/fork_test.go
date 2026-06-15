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

type fakeForkClient struct {
	entries    []*cpv1.JournalKeyCiphertext
	nodeID     string
	nodePub    []byte
	gen        uint64
	notAfter   time.Time
	forkID     string
	transferID string
	forkErr    error
	deliverErr error

	gotFork           *cpv1.ForkSpawnRequest
	gotJournalSpawnID string
	gotNodeKeySpawnID string
	gotDelivery       *cpv1.DeliverSecretsRequest
}

func (f *fakeForkClient) ForkSpawn(_ context.Context, req *connect.Request[cpv1.ForkSpawnRequest]) (*connect.Response[cpv1.ForkSpawnResponse], error) {
	f.gotFork = req.Msg
	if f.forkErr != nil {
		return nil, f.forkErr
	}
	return connect.NewResponse(&cpv1.ForkSpawnResponse{
		ForkSpawnId:   f.forkID,
		NodeId:        f.nodeID,
		TransferSetId: f.transferID,
	}), nil
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

func TestForkTargetMapping(t *testing.T) {
	if r, err := forkTarget("sp1", "", "", ""); err != nil || r.TargetNodeId != "" || r.TargetClass != "" {
		t.Fatalf("default target = %+v err=%v", r, err)
	}
	if r, err := forkTarget("sp1", "node-b", "", "Trial"); err != nil || r.TargetNodeId != "node-b" || r.Name != "Trial" {
		t.Fatalf("node target = %+v err=%v", r, err)
	}
	if r, err := forkTarget("sp1", "", "cloud", ""); err != nil || r.TargetClass != "cloud" || r.TargetNodeId != "" {
		t.Fatalf("class target = %+v err=%v", r, err)
	}
	if _, err := forkTarget("sp1", "node-b", "cloud", ""); err == nil {
		t.Fatal("node and class together must be rejected")
	}
}

func TestRunForkCallsForkSpawnDefaultSameNode(t *testing.T) {
	dev := testForkDevice(t)
	client := &fakeForkClient{forkID: "fork-1", nodeID: "node-a", transferID: "ts-1"}
	var out bytes.Buffer

	if err := runFork(context.Background(), client, dev, "source-1", "", "", "", &out, time.Now()); err != nil {
		t.Fatalf("runFork: %v", err)
	}

	if client.gotFork == nil || client.gotFork.SpawnId != "source-1" || client.gotFork.TargetNodeId != "" || client.gotFork.TargetClass != "" {
		t.Fatalf("ForkSpawn req = %+v", client.gotFork)
	}
	if client.gotJournalSpawnID != "fork-1" {
		t.Fatalf("journal ciphertext requested for %q, want fork-1", client.gotJournalSpawnID)
	}
	if !strings.Contains(out.String(), "same node") || !strings.Contains(out.String(), "fork fork-1 active on node node-a") {
		t.Fatalf("output missing fork summary:\n%s", out.String())
	}
}

func TestRunForkPassesNameAndNode(t *testing.T) {
	dev := testForkDevice(t)
	client := &fakeForkClient{forkID: "fork-2", nodeID: "node-b"}
	var out bytes.Buffer

	if err := runFork(context.Background(), client, dev, "source-1", "node-b", "", " Trial ", &out, time.Now()); err != nil {
		t.Fatalf("runFork: %v", err)
	}

	if client.gotFork == nil || client.gotFork.TargetNodeId != "node-b" || client.gotFork.TargetClass != "" || client.gotFork.Name != "Trial" {
		t.Fatalf("ForkSpawn req = %+v, want node-b + trimmed name", client.gotFork)
	}
}

func TestRunForkOwnerSealedDeliveryUsesForkID(t *testing.T) {
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
	if err := runFork(context.Background(), client, dev, "source-1", "node-b", "", "", &out, time.Now()); err != nil {
		t.Fatalf("runFork: %v", err)
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
}

func TestRunForkDeliveryFailureReportsPending(t *testing.T) {
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
	err = runFork(context.Background(), client, dev, "source-1", "node-b", "", "", &out, time.Now())
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
