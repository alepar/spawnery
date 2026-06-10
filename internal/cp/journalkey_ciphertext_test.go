package cp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/journalkeys"
	"spawnery/internal/cp/store"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
)

// seedSpawnWithMount inserts a (suspended-capable) spawn owned by owner with a single named mount, so
// the journal-key ciphertext RPCs (which enumerate the spawn's mounts) have a row to serve.
func seedSpawnWithMount(t *testing.T, s *Server, id, owner, mount string) {
	t.Helper()
	ctx := context.Background()
	if err := s.st.Owners().Upsert(ctx, store.Owner{ID: owner, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	sp := store.Spawn{
		ID: id, OwnerID: owner, AppID: "secret-app", AppVersion: "1.0.0", AppRef: "examples/secret-app",
		Model: "m", CreatedAt: 1, LastUsedAt: 1,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		return tx.Spawns().Create(ctx, sp, []store.Mount{{Name: mount, BackendURI: "scratch"}})
	}); err != nil {
		t.Fatal(err)
	}
}

// ownerSealedCiphertext seals password to dev's device pubkey and returns the JSON envelope bytes the
// CP custodies (ciphertext only).
func ownerSealedCiphertext(t *testing.T, password, owner, mount string, dev *seal.Device) []byte {
	t.Helper()
	env, err := journalkey.SealToOwner(password, []seal.X25519PubKey{dev.X25519PubKey()},
		seal.AtRestAAD{AccountID: owner, SecretID: journalkey.SecretID(mount), Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Put then Get round-trips the owner-sealed ciphertext unchanged, and is owner-only.
func TestJournalKeyCiphertextPutGetAuth(t *testing.T) {
	s, _, _ := newTestServer(t)
	devReg := journalkeys.NewMemDeviceRegistry()
	s.ownerDevices = devReg
	seedSpawnWithMount(t, s, "sp1", "alice", "main")

	m, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(m, "")
	devReg.Enroll("alice", dev.X25519PubKey())
	ct := ownerSealedCiphertext(t, "repo-pw", "alice", "main", dev)

	alice := auth.WithOwner(context.Background(), "alice")
	mallory := auth.WithOwner(context.Background(), "mallory")

	// Non-owner Put is rejected.
	if _, err := s.PutJournalKeyCiphertext(mallory, connect.NewRequest(&cpv1.PutJournalKeyCiphertextRequest{
		SpawnId: "sp1", Entries: []*cpv1.JournalKeyCiphertext{{Mount: "main", Ciphertext: ct}},
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-owner Put: want PermissionDenied, got %v", err)
	}
	// Empty entries rejected.
	if _, err := s.PutJournalKeyCiphertext(alice, connect.NewRequest(&cpv1.PutJournalKeyCiphertextRequest{SpawnId: "sp1"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty Put: want InvalidArgument, got %v", err)
	}
	// Owner Put succeeds.
	if _, err := s.PutJournalKeyCiphertext(alice, connect.NewRequest(&cpv1.PutJournalKeyCiphertextRequest{
		SpawnId: "sp1", Entries: []*cpv1.JournalKeyCiphertext{{Mount: "main", Ciphertext: ct}},
	})); err != nil {
		t.Fatalf("owner Put: %v", err)
	}
	// Non-owner Get is rejected.
	if _, err := s.GetJournalKeyCiphertext(mallory, connect.NewRequest(&cpv1.GetJournalKeyCiphertextRequest{SpawnId: "sp1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-owner Get: want PermissionDenied, got %v", err)
	}
	// Owner Get returns the ciphertext unchanged.
	resp, err := s.GetJournalKeyCiphertext(alice, connect.NewRequest(&cpv1.GetJournalKeyCiphertextRequest{SpawnId: "sp1"}))
	if err != nil {
		t.Fatalf("owner Get: %v", err)
	}
	if len(resp.Msg.Entries) != 1 || resp.Msg.Entries[0].Mount != "main" || string(resp.Msg.Entries[0].Ciphertext) != string(ct) {
		t.Fatalf("Get = %+v, want one main entry with the stored ciphertext", resp.Msg.Entries)
	}
}

// Put fails closed when the uploaded envelope is sealed to NONE of the owner's enrolled devices (the
// CP must never custody ciphertext the owner cannot later open).
func TestPutJournalKeyCiphertextRejectsForeignSeal(t *testing.T) {
	s, _, _ := newTestServer(t)
	devReg := journalkeys.NewMemDeviceRegistry()
	s.ownerDevices = devReg
	seedSpawnWithMount(t, s, "sp1", "alice", "main")

	// Alice's enrolled device.
	m1, _ := seal.NewMnemonic()
	aliceDev, _ := seal.DeviceFromMnemonic(m1, "")
	devReg.Enroll("alice", aliceDev.X25519PubKey())
	// Ciphertext sealed to a DIFFERENT device (not enrolled).
	m2, _ := seal.NewMnemonic()
	otherDev, _ := seal.DeviceFromMnemonic(m2, "")
	foreign := ownerSealedCiphertext(t, "repo-pw", "alice", "main", otherDev)

	alice := auth.WithOwner(context.Background(), "alice")
	if _, err := s.PutJournalKeyCiphertext(alice, connect.NewRequest(&cpv1.PutJournalKeyCiphertextRequest{
		SpawnId: "sp1", Entries: []*cpv1.JournalKeyCiphertext{{Mount: "main", Ciphertext: foreign}},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("foreign-sealed Put: want InvalidArgument, got %v", err)
	}
}

// The journal key, fetched from the CP, re-seals to node B's HPKE sub-key and node B opens it to
// recover the original repo password — the cross-node restore key travel (design §4), validated at the
// crypto level (a real 2-node e2e is out of scope here).
func TestJournalKeyResealToNodeBOpens(t *testing.T) {
	s, _, _ := newTestServer(t)
	devReg := journalkeys.NewMemDeviceRegistry()
	s.ownerDevices = devReg
	seedSpawnWithMount(t, s, "sp1", "alice", "main")

	m, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(m, "")
	devReg.Enroll("alice", dev.X25519PubKey())
	const password = "kopia-repo-password-xyz"
	ct := ownerSealedCiphertext(t, password, "alice", "main", dev)

	alice := auth.WithOwner(context.Background(), "alice")
	if _, err := s.PutJournalKeyCiphertext(alice, connect.NewRequest(&cpv1.PutJournalKeyCiphertextRequest{
		SpawnId: "sp1", Entries: []*cpv1.JournalKeyCiphertext{{Mount: "main", Ciphertext: ct}},
	})); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJournalKeyCiphertext(alice, connect.NewRequest(&cpv1.GetJournalKeyCiphertextRequest{SpawnId: "sp1"}))
	if err != nil {
		t.Fatal(err)
	}
	fetched := got.Msg.Entries[0].Ciphertext

	// Owner-client leg: unseal + reseal to node B.
	var env seal.Envelope
	if err := json.Unmarshal(fetched, &env); err != nil {
		t.Fatal(err)
	}
	bPub, bPriv, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	aad := seal.InFlightAAD{
		SpawnID: "sp1", Generation: 2, NodeID: "node-b",
		NotAfter: time.Now().Add(time.Hour), Version: 2, DeliveryID: "delivery-1",
	}
	sealed, err := journalkey.ResealForNode(&env, dev.X25519Priv, bPub, aad)
	if err != nil {
		t.Fatal(err)
	}
	// Node B leg: open the delivered ciphertext and recover the password.
	recovered, err := seal.OpenFromOwner(sealed, bPriv, aad, time.Now())
	if err != nil {
		t.Fatalf("node B OpenFromOwner: %v", err)
	}
	if string(recovered) != password {
		t.Fatalf("node B recovered %q, want %q", recovered, password)
	}
}
