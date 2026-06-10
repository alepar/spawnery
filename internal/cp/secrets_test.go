package cp

import (
	"bytes"
	"context"
	"testing"
	"time"

	connect "connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// createActiveSpawn inserts an active spawn owned by `owner`, bound to nodeID at generation 1, and
// returns its id. The live container row is what GetSpawnNodeKey/DeliverSecrets resolve the node from.
func createActiveSpawn(t *testing.T, s *Server, owner, spawnID, nodeID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	sp := store.Spawn{
		ID: spawnID, OwnerID: owner, Name: "n", AppID: "secret-app", AppVersion: "1.0.0",
		AppRef: "examples/secret-app", Model: "m", CreatedAt: now, LastUsedAt: now,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, nil) }); err != nil {
		t.Fatal(err)
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().SetActive(ctx, spawnID, nodeID, 1) }); err != nil {
		t.Fatal(err)
	}
}

// secretDeliveries returns every SecretDelivery the sender was asked to relay.
func (c *capSender) secretDeliveries() []*nodev1.SecretDelivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*nodev1.SecretDelivery
	for _, m := range c.sent {
		if sd := m.GetSecretDelivery(); sd != nil {
			out = append(out, sd)
		}
	}
	return out
}

// GetSpawnNodeKey returns the sub-key the node published on Register (cached by the CP), plus the
// spawn's live generation, to the owner. The CP relays the bytes opaquely.
func TestGetSpawnNodeKeyReturnsPublishedSubKey(t *testing.T) {
	s, _, _ := newTestServer(t)
	subkeyJSON := []byte(`{"hpke_pub":"AAAA","node_id":"n1","not_after":"2099-01-01T00:00:00Z"}`)

	in := make(chan *nodev1.NodeMessage, 2)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: "n1", MaxSpawns: 1, SignedSubkey: subkeyJSON}}}
	// Wait for the Register to land the sub-key in the cache.
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := s.nodeKeys.get("n1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node sub-key never cached")
		}
		time.Sleep(time.Millisecond)
	}

	createActiveSpawn(t, s, "alice", "sp1", "n1")
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.GetSpawnNodeKey(ctx, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: "sp1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resp.Msg.SignedSubkey, subkeyJSON) {
		t.Fatalf("SignedSubkey = %q, want %q", resp.Msg.SignedSubkey, subkeyJSON)
	}
	if resp.Msg.Generation != 1 {
		t.Fatalf("Generation = %d, want 1", resp.Msg.Generation)
	}
	close(in)
}

// GetSpawnNodeKey rejects a non-owner (ownership-checked) and reports FailedPrecondition when the
// hosting node has published no sub-key.
func TestGetSpawnNodeKeyAuthAndPreconditions(t *testing.T) {
	s, _, _ := newTestServer(t)
	createActiveSpawn(t, s, "alice", "sp1", "n1")

	// Non-owner: PermissionDenied.
	mallory := auth.WithOwner(context.Background(), "mallory")
	if _, err := s.GetSpawnNodeKey(mallory, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: "sp1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-owner GetSpawnNodeKey: want PermissionDenied, got %v", err)
	}
	// Owner but the node never published a sub-key: FailedPrecondition.
	alice := auth.WithOwner(context.Background(), "alice")
	if _, err := s.GetSpawnNodeKey(alice, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: "sp1"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("no-subkey GetSpawnNodeKey: want FailedPrecondition, got %v", err)
	}
}

// DeliverSecrets relays the owner's ciphertext to the hosting node UNCHANGED (the CP never unseals):
// the relayed SecretDelivery carries the exact `sealed` bytes, the live generation, and target path.
func TestDeliverSecretsRelaysOpaqueCiphertext(t *testing.T) {
	s, _, rt := newTestServer(t)
	createActiveSpawn(t, s, "alice", "sp1", "n1")
	sender := &capSender{}
	rt.Bind("sp1", "n1", sender)

	ciphertext := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01} // opaque to the CP
	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.DeliverSecrets(ctx, connect.NewRequest(&cpv1.DeliverSecretsRequest{
		SpawnId: "sp1",
		Secrets: []*cpv1.SealedSecret{{TargetPath: "gh/hosts.yml", Sealed: ciphertext, SecretId: "gh"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := sender.secretDeliveries()
	if len(got) != 1 {
		t.Fatalf("relayed %d SecretDelivery messages, want 1", len(got))
	}
	sd := got[0]
	if sd.SpawnId != "sp1" || sd.Generation != 1 {
		t.Fatalf("relayed delivery wrong spawn/gen: %+v", sd)
	}
	if len(sd.Secrets) != 1 {
		t.Fatalf("relayed %d secrets, want 1", len(sd.Secrets))
	}
	if !bytes.Equal(sd.Secrets[0].Sealed, ciphertext) {
		t.Fatalf("CP must relay ciphertext OPAQUELY: got %x, want %x", sd.Secrets[0].Sealed, ciphertext)
	}
	if sd.Secrets[0].TargetPath != "gh/hosts.yml" || sd.Secrets[0].SecretId != "gh" {
		t.Fatalf("relayed secret metadata mangled: %+v", sd.Secrets[0])
	}
}

// DeliverSecrets is owner-only + ownership-checked, rejects an empty secret set, and reports Unavailable
// when the spawn has no live node route.
func TestDeliverSecretsAuthAndValidation(t *testing.T) {
	s, _, rt := newTestServer(t)
	createActiveSpawn(t, s, "alice", "sp1", "n1")

	// Non-owner: PermissionDenied (and no relay).
	mallory := auth.WithOwner(context.Background(), "mallory")
	if _, err := s.DeliverSecrets(mallory, connect.NewRequest(&cpv1.DeliverSecretsRequest{
		SpawnId: "sp1", Secrets: []*cpv1.SealedSecret{{TargetPath: "x", Sealed: []byte("c"), SecretId: "s"}},
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-owner DeliverSecrets: want PermissionDenied, got %v", err)
	}

	alice := auth.WithOwner(context.Background(), "alice")
	// Empty secret set: InvalidArgument.
	if _, err := s.DeliverSecrets(alice, connect.NewRequest(&cpv1.DeliverSecretsRequest{SpawnId: "sp1"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty DeliverSecrets: want InvalidArgument, got %v", err)
	}
	// No route bound: Unavailable (the owner retries after a resume).
	if _, err := s.DeliverSecrets(alice, connect.NewRequest(&cpv1.DeliverSecretsRequest{
		SpawnId: "sp1", Secrets: []*cpv1.SealedSecret{{TargetPath: "x", Sealed: []byte("c"), SecretId: "s"}},
	})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("unrouted DeliverSecrets: want Unavailable, got %v", err)
	}
	_ = rt
}
