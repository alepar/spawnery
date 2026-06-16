package cp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	connect "connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
	"spawnery/internal/secrets/journalkey"
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

func createPendingFork(t *testing.T, s *Server, owner, sourceID, forkID, sourceNodeID, targetNodeID string, sourceGeneration, targetGeneration uint64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	if err := s.st.Owners().Upsert(ctx, store.Owner{ID: owner, CreatedAt: now}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	source := store.Spawn{
		ID: sourceID, OwnerID: owner, Name: "source", AppID: "secret-app", AppVersion: "1.0.0",
		AppRef: "examples/secret-app", Model: "m", CreatedAt: now, LastUsedAt: now, Status: store.Active,
	}
	parentID := sourceID
	forkedAt := now
	fork := store.Spawn{
		ID: forkID, OwnerID: owner, Name: "fork", AppID: source.AppID, AppVersion: source.AppVersion,
		AppRef: source.AppRef, Model: source.Model, CreatedAt: now, LastUsedAt: now, Status: store.Starting,
		ParentSpawnID: &parentID, ForkedAt: &forkedAt,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.Spawns().Create(ctx, source, []store.Mount{{Name: "main", BackendURI: "scratch"}}); err != nil {
			return err
		}
		if err := tx.Spawns().Create(ctx, fork, []store.Mount{{Name: "main", BackendURI: "scratch"}}); err != nil {
			return err
		}
		return tx.TransferSets().Create(ctx, store.TransferSet{
			ID:                "ts-" + forkID,
			Kind:              store.TransferSetFork,
			SpawnID:           forkID,
			SourceSpawnID:     sourceID,
			ForkSpawnID:       forkID,
			SourceGeneration:  sourceGeneration,
			TargetGeneration:  targetGeneration,
			SourceNodeID:      sourceNodeID,
			TargetNodeID:      targetNodeID,
			TransferKeyStatus: store.TransferKeyTargetReady,
			Status:            store.TransferSetRestoring,
			CreatedAt:         now,
			UpdatedAt:         now,
		})
	}); err != nil {
		t.Fatalf("seed pending fork: %v", err)
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

func TestGetPendingIntentReturnsTargetNodeSubKey(t *testing.T) {
	s, _, stopACK := intentTestServer(t)
	defer stopACK()
	subkeyJSON := []byte(`{"hpke_pub":"AAAA","node_id":"n-intent","not_after":"2099-01-01T00:00:00Z"}`)
	s.nodeKeys.put("n-intent", subkeyJSON, []byte("cert-chain"))

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatal(err)
	}
	spawnID := resp.Msg.GetSpawnId()

	var pending *cpv1.PendingIntent
	deadline := time.Now().Add(time.Second)
	for {
		got, err := s.GetPendingIntent(ctx, connect.NewRequest(&cpv1.GetPendingIntentRequest{SpawnId: spawnID}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Msg.GetReady() {
			pending = got.Msg.GetPending()
			if !bytes.Equal(got.Msg.GetSignedSubkey(), subkeyJSON) {
				t.Fatalf("SignedSubkey = %q, want %q", got.Msg.GetSignedSubkey(), subkeyJSON)
			}
			if string(got.Msg.GetNodeCertChain()) != "cert-chain" {
				t.Fatalf("NodeCertChain = %q, want cert-chain", string(got.Msg.GetNodeCertChain()))
			}
			if got.Msg.GetGeneration() != pending.GetGeneration() {
				t.Fatalf("Generation = %d, want pending generation %d", got.Msg.GetGeneration(), pending.GetGeneration())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending intent never became ready")
		}
		time.Sleep(time.Millisecond)
	}

	sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	errCh := make(chan error, 1)
	goSubmitIntent(context.Background(), s, spawnID, "alice", sessionKey, errCh)
	if submitErr := <-errCh; submitErr != nil {
		t.Fatalf("SubmitIntent cleanup: %v", submitErr)
	}
}

func TestGetSpawnNodeKeyReturnsPendingForkTargetSubKey(t *testing.T) {
	s, _, _ := newTestServer(t)
	subkeyJSON := []byte(`{"hpke_pub":"BBBB","node_id":"node-2","not_after":"2099-01-01T00:00:00Z"}`)
	certChain := []byte("node-2-cert-chain")
	s.nodeKeys.put("node-2", subkeyJSON, certChain)
	createPendingFork(t, s, "alice", "sp-source", "sp-fork", "node-1", "node-2", 9, 1)

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.GetSpawnNodeKey(ctx, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: "sp-fork"}))
	if err != nil {
		t.Fatalf("GetSpawnNodeKey pending fork: %v", err)
	}
	if !bytes.Equal(resp.Msg.SignedSubkey, subkeyJSON) || !bytes.Equal(resp.Msg.NodeCertChain, certChain) {
		t.Fatalf("pending fork key material = subkey %q cert %q", resp.Msg.SignedSubkey, resp.Msg.NodeCertChain)
	}
	if resp.Msg.Generation != 1 {
		t.Fatalf("pending fork generation = %d, want 1", resp.Msg.Generation)
	}

	mallory := auth.WithOwner(context.Background(), "mallory")
	if _, err := s.GetSpawnNodeKey(mallory, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: "sp-fork"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-owner pending fork GetSpawnNodeKey: want PermissionDenied, got %v", err)
	}
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

func TestStartupSecretIDsFromArtifactsAndValidation(t *testing.T) {
	arts := []store.Artifact{
		{Sensitive: true, EnvVarName: "gh-main"},
		{Sensitive: true, EnvVarName: "  byok  "},
		{Sensitive: true, EnvVarName: "gh-main"},
		{Sensitive: false, EnvVarName: "plain"},
		{Sensitive: true},
	}
	required := startupSecretIDsFromArtifacts(arts)
	if want := []string{"byok", "gh-main"}; !equalStringSlices(required, want) {
		t.Fatalf("startupSecretIDsFromArtifacts = %+v, want %+v", required, want)
	}

	tests := []struct {
		name    string
		req     []string
		got     []*nodev1.SealedSecret
		wantErr bool
	}{
		{name: "none required none submitted", req: nil, got: nil},
		{name: "all required submitted", req: []string{"byok", "gh-main"}, got: []*nodev1.SealedSecret{{SecretId: "gh-main", Sealed: []byte("ciphertext")}, {SecretId: "byok", Sealed: []byte("ciphertext")}}},
		{name: "nil submitted secret", req: []string{"gh-main"}, got: []*nodev1.SealedSecret{nil}, wantErr: true},
		{name: "empty submitted id", req: []string{"gh-main"}, got: []*nodev1.SealedSecret{{SecretId: "", Sealed: []byte("ciphertext")}}, wantErr: true},
		{name: "empty sealed payload", req: []string{"gh-main"}, got: []*nodev1.SealedSecret{{SecretId: "gh-main"}}, wantErr: true},
		{name: "duplicate submitted id", req: []string{"gh-main"}, got: []*nodev1.SealedSecret{{SecretId: "gh-main", Sealed: []byte("ciphertext")}, {SecretId: "gh-main", Sealed: []byte("ciphertext")}}, wantErr: true},
		{name: "undeclared submitted id", req: []string{"gh-main"}, got: []*nodev1.SealedSecret{{SecretId: "gh-main", Sealed: []byte("ciphertext")}, {SecretId: "extra", Sealed: []byte("ciphertext")}}, wantErr: true},
		{name: "missing required id", req: []string{"gh-main"}, got: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSubmittedStartupSecrets(tt.req, tt.got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSubmittedStartupSecrets() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
		Secrets: []*cpv1.SealedSecret{{
			TargetPath: "gh/hosts.yml",
			Sealed:     ciphertext,
			SecretId:   "gh",
			Version:    11,
			DeliveryId: "delivery-gh-v11",
		}},
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
	if sd.Secrets[0].Version != 11 || sd.Secrets[0].DeliveryId != "delivery-gh-v11" {
		t.Fatalf("relayed delivery metadata = version %d id %q, want version 11 id delivery-gh-v11", sd.Secrets[0].Version, sd.Secrets[0].DeliveryId)
	}
}

func TestDeliverSecretsRelaysOpaqueCiphertextToPendingForkTargetAndClearsJournalPending(t *testing.T) {
	s, reg, _ := newTestServer(t)
	createPendingFork(t, s, "alice", "sp-source", "sp-fork", "node-1", "node-2", 9, 1)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "node-2", Sender: sender, Max: 1, Free: 1, Class: "cloud"})
	s.deliveryPending.mark("sp-fork")

	ciphertext := []byte{0xCA, 0xFE, 0x00, 0x02}
	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.DeliverSecrets(ctx, connect.NewRequest(&cpv1.DeliverSecretsRequest{
		SpawnId: "sp-fork",
		Secrets: []*cpv1.SealedSecret{{
			TargetPath: "journal/main",
			Sealed:     ciphertext,
			SecretId:   journalkey.SecretID("main"),
		}},
	}))
	if err != nil {
		t.Fatalf("DeliverSecrets pending fork: %v", err)
	}
	got := sender.secretDeliveries()
	if len(got) != 1 {
		t.Fatalf("relayed %d SecretDelivery messages, want 1", len(got))
	}
	sd := got[0]
	if sd.SpawnId != "sp-fork" || sd.Generation != 1 {
		t.Fatalf("pending fork delivery wrong spawn/gen: %+v", sd)
	}
	if len(sd.Secrets) != 1 || !bytes.Equal(sd.Secrets[0].Sealed, ciphertext) ||
		sd.Secrets[0].SecretId != journalkey.SecretID("main") {
		t.Fatalf("pending fork delivery mangled ciphertext/metadata: %+v", sd.Secrets)
	}
	if s.deliveryPending.isPending("sp-fork") {
		t.Fatal("journal-key delivery must clear pending state for the fork")
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
