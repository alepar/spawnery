package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage/journal"
)

// testSignKey generates a P-256 signing key for a node sub-key holder (the node cert key type).
func testSignKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// secretTestRig builds a node attacher with a real HPKE sub-key holder, a manager backed by a fake pod
// backend, and a started spawn (so InjectSecret can resolve it). It returns the attacher, the holder,
// the secrets root dir, and the spawn id. nodeID is the holder's identity (bound into the delivery AAD).
func secretTestRig(t *testing.T, nodeID, spawnID string, gen uint64) (*attacher, *subkey.Node, string) {
	t.Helper()
	holder := subkey.NewNode(testSignKey(t), nodeID, 0)
	if _, err := holder.Rotate(time.Now()); err != nil {
		t.Fatal(err)
	}

	dataRoot := t.TempDir()
	mgr := spawnlet.NewManagerWithBackend(&scriptedPodBackend{}, noopApplier{}, spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot,
	})
	// Put the spawn in the manager's store + create its secrets dir (Create does both).
	if _, err := mgr.Create(context.Background(), spawnID, writeNodeApp(t), "m", "name", "app", gen); err != nil {
		t.Fatal(err)
	}

	a := newAttacher(mgr, &fakeCPStream{})
	a.cfg.NodeID = nodeID
	a.cfg.SubKeys = holder
	return a, holder, filepath.Join(dataRoot, "secrets")
}

// sealSecret performs the OWNER-side leg: fresh-DEK envelope sealed to a device key, then re-sealed to
// the node's published HPKE sub-key under the in-flight AAD — exactly internal/secrets/seal +
// internal/secrets/subkey as a real owner client would, JSON-encoded onto a wire SealedSecret.
func sealSecret(t *testing.T, holder *subkey.Node, spawnID string, gen uint64, target, secretID string, version uint64, deliveryID string, payload []byte) *nodev1.SealedSecret {
	t.Helper()
	devPub, devPriv, err := seal.NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	env, err := seal.Seal(payload, []seal.X25519PubKey{devPub}, seal.AtRestAAD{AccountID: "alice", SecretID: secretID, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	signed, ok := holder.Current(time.Now())
	if !ok {
		t.Fatal("holder has no current sub-key")
	}
	aad := seal.InFlightAAD{
		SpawnID:    spawnID,
		Generation: gen,
		NodeID:     signed.NodeID,
		NotAfter:   signed.NotAfter,
		Version:    version,
		DeliveryID: deliveryID,
	}
	ns, err := seal.ReSealToNode(env, devPriv, signed.HPKEPub, aad)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(ns)
	if err != nil {
		t.Fatal(err)
	}
	return &nodev1.SealedSecret{TargetPath: target, Sealed: b, SecretId: secretID, Version: version, DeliveryId: deliveryID}
}

// Full round trip: seal with seal+subkey -> the wire SecretDelivery the CP relays -> node unseals ->
// plaintext lands in the spawn's tmpfs secrets dir at the declared path, mode 0600, exact content.
func TestSecretDeliveryUnsealsAndWritesTmpfs(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(7)
	a, holder, secretsRoot := secretTestRig(t, nodeID, spawnID, gen)
	want := []byte("ghp_supersecrettoken\n")
	sec := sealSecret(t, holder, spawnID, gen, "gh/hosts.yml", "gh", 1, "d-gh", want)

	a.handleSecretDelivery(&nodev1.SecretDelivery{SpawnId: spawnID, Generation: gen, Secrets: []*nodev1.SealedSecret{sec}})

	path := filepath.Join(secretsRoot, spawnID, "gh", "hosts.yml")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("secret file not written at %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// A SecretDelivery whose generation is below the live pod's is stale (it targets a superseded episode)
// and must be dropped by the handle() fence — no file written.
func TestSecretDeliveryStaleGenerationDropped(t *testing.T) {
	const nodeID, spawnID, liveGen = "node-1", "sp1", uint64(7)
	a, holder, secretsRoot := secretTestRig(t, nodeID, spawnID, liveGen)
	// Owner sealed for a now-superseded episode (gen 6 < live 7).
	sec := sealSecret(t, holder, spawnID, 6, "gh/hosts.yml", "gh", 1, "d-gh-stale", []byte("stale"))

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_SecretDelivery{
		SecretDelivery: &nodev1.SecretDelivery{SpawnId: spawnID, Generation: 6, Secrets: []*nodev1.SealedSecret{sec}},
	}})

	// handle dispatches async; give the (fenced) goroutine a chance to run, then assert nothing landed.
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(secretsRoot, spawnID, "gh", "hosts.yml")); !os.IsNotExist(err) {
		t.Fatalf("stale-generation delivery must be dropped, but a file was written (err=%v)", err)
	}
}

// A ciphertext sealed for a DIFFERENT context (wrong generation in the AAD, but relayed at the live
// generation) fails the HPKE AAD check on Open and is not written — a CP cannot replay across episodes.
func TestSecretDeliveryWrongContextRejected(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(7)
	a, holder, secretsRoot := secretTestRig(t, nodeID, spawnID, gen)
	// Sealed with AAD generation 99, but delivered as generation 7 (the live one): AAD mismatch.
	sec := sealSecret(t, holder, spawnID, 99, "gh/hosts.yml", "gh", 1, "d-gh-wrong-context", []byte("nope"))

	a.handleSecretDelivery(&nodev1.SecretDelivery{SpawnId: spawnID, Generation: gen, Secrets: []*nodev1.SealedSecret{sec}})

	if _, err := os.Stat(filepath.Join(secretsRoot, spawnID, "gh", "hosts.yml")); !os.IsNotExist(err) {
		t.Fatalf("wrong-context ciphertext must be rejected, but a file was written (err=%v)", err)
	}
}

// A node with no sub-key holder (insecure/dev mode) drops the delivery rather than panicking.
func TestSecretDeliveryNoHolderDropped(t *testing.T) {
	mgr := spawnlet.NewManagerWithBackend(&scriptedPodBackend{}, noopApplier{}, spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	if _, err := mgr.Create(context.Background(), "sp1", writeNodeApp(t), "m", "n", "app", 1); err != nil {
		t.Fatal(err)
	}
	a := newAttacher(mgr, &fakeCPStream{}) // no SubKeys
	a.handleSecretDelivery(&nodev1.SecretDelivery{SpawnId: "sp1", Generation: 1, Secrets: []*nodev1.SealedSecret{{TargetPath: "x", Sealed: []byte("{}"), SecretId: "s"}}})
	// No panic, no file.
}

// A journal-key delivery (secret_id prefix journalkey.Prefix) carries the per-spawn Kopia repo password,
// not a tmpfs secret: it must be routed into the journaler's owner-sealed custody (so a cross-node resume
// can open the repo) and NOT written to the secrets tmpfs. This is the node-side leg of sp-u53.5.4.
func TestSecretDeliveryRoutesJournalKeyToCustody(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(7)
	a, holder, secretsRoot := secretTestRig(t, nodeID, spawnID, gen)

	// Wire an owner-sealed journaler into the manager and keep a handle to the custody.
	jroot := t.TempDir()
	keyfile := filepath.Join(jroot, "node.key")
	if err := journal.GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatal(err)
	}
	nl, err := journal.NewNodeLocalCustody(keyfile, filepath.Join(jroot, "seals"))
	if err != nil {
		t.Fatal(err)
	}
	osc := journal.NewOwnerSealedCustody()
	jm, err := journal.NewManager(journal.Config{
		RepoRoot:    filepath.Join(jroot, "repos"),
		Backend:     &journal.FilesystemBackend{Root: filepath.Join(jroot, "blobs")},
		Custody:     nl,
		OwnerSealed: osc,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.mgr.SetJournal(jm, filepath.Join(jroot, "state"))

	const repoPW = "kopia-repo-pw-delivered-cross-node"
	sec := sealSecret(t, holder, spawnID, gen, "", journalkey.SecretID("work"), 1, "d-journal-work", []byte(repoPW))
	a.handleSecretDelivery(&nodev1.SecretDelivery{SpawnId: spawnID, Generation: gen, Secrets: []*nodev1.SealedSecret{sec}})

	got, err := osc.PasswordFor(spawnID)
	if err != nil || got != repoPW {
		t.Fatalf("journal key not delivered to custody: got %q, err %v", got, err)
	}
	if g, ok := osc.Delivered(spawnID); !ok || g != gen {
		t.Fatalf("delivered generation = (%d, %v), want (%d, true)", g, ok, gen)
	}
	// It must NOT have been written to the secrets tmpfs.
	entries, _ := os.ReadDir(filepath.Join(secretsRoot, spawnID))
	if len(entries) != 0 {
		t.Fatalf("journal key must not be written to tmpfs; found %d entries", len(entries))
	}
}

// A journal-key delivery for a node with NO owner-sealed journaler configured is dropped (logged), not
// panicked — and obviously lands nowhere.
func TestSecretDeliveryJournalKeyNoJournalerDropped(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(3)
	a, holder, _ := secretTestRig(t, nodeID, spawnID, gen) // rig manager has no journaler
	sec := sealSecret(t, holder, spawnID, gen, "", journalkey.SecretID("work"), 1, "d-journal-work-no-journaler", []byte("pw"))
	// Must not panic.
	a.handleSecretDelivery(&nodev1.SecretDelivery{SpawnId: spawnID, Generation: gen, Secrets: []*nodev1.SealedSecret{sec}})
}

func TestSecretDeliveryUsesWireVersionAndDeliveryIDForAAD(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(7)
	a, holder, secretsRoot := secretTestRig(t, nodeID, spawnID, gen)

	sec := sealSecret(t, holder, spawnID, gen, "gh/hosts.yml", "gh", 11, "delivery-sp1-gh-v11", []byte("token-v11"))
	a.handleSecretDelivery(&nodev1.SecretDelivery{SpawnId: spawnID, Generation: gen, Secrets: []*nodev1.SealedSecret{sec}})

	got, err := os.ReadFile(filepath.Join(secretsRoot, spawnID, "gh", "hosts.yml"))
	if err != nil {
		t.Fatalf("secret file not written: %v", err)
	}
	if string(got) != "token-v11" {
		t.Fatalf("content = %q, want token-v11", got)
	}
}

func TestSecretDeliveryRejectsDuplicateDeliveryID(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(7)
	a, holder, secretsRoot := secretTestRig(t, nodeID, spawnID, gen)

	first := sealSecret(t, holder, spawnID, gen, "gh/hosts.yml", "gh", 3, "delivery-dup", []byte("first"))
	replay := sealSecret(t, holder, spawnID, gen, "gh/hosts.yml", "gh", 3, "delivery-dup", []byte("second"))

	a.handleSecretDelivery(&nodev1.SecretDelivery{SpawnId: spawnID, Generation: gen, Secrets: []*nodev1.SealedSecret{first}})
	a.handleSecretDelivery(&nodev1.SecretDelivery{SpawnId: spawnID, Generation: gen, Secrets: []*nodev1.SealedSecret{replay}})

	got, err := os.ReadFile(filepath.Join(secretsRoot, spawnID, "gh", "hosts.yml"))
	if err != nil {
		t.Fatalf("secret file not written: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("duplicate delivery overwrote content: got %q, want first", got)
	}
}

func TestSecretDeliveryRejectsOlderVersionAfterNewerAccepted(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(7)
	a, holder, secretsRoot := secretTestRig(t, nodeID, spawnID, gen)

	newer := sealSecret(t, holder, spawnID, gen, "gh/hosts.yml", "gh", 5, "delivery-v5", []byte("newer"))
	older := sealSecret(t, holder, spawnID, gen, "gh/hosts.yml", "gh", 4, "delivery-v4", []byte("older"))

	a.handleSecretDelivery(&nodev1.SecretDelivery{SpawnId: spawnID, Generation: gen, Secrets: []*nodev1.SealedSecret{newer}})
	a.handleSecretDelivery(&nodev1.SecretDelivery{SpawnId: spawnID, Generation: gen, Secrets: []*nodev1.SealedSecret{older}})

	got, err := os.ReadFile(filepath.Join(secretsRoot, spawnID, "gh", "hosts.yml"))
	if err != nil {
		t.Fatalf("secret file not written: %v", err)
	}
	if string(got) != "newer" {
		t.Fatalf("older version overwrote content: got %q, want newer", got)
	}
}

func TestSecretDeliveryReplaySerializesSameSecretReservations(t *testing.T) {
	r := newSecretDeliveryReplay()
	_, oldRollback, err := r.begin("sp1", 7, "gh", 4, "delivery-v4")
	if err != nil {
		t.Fatalf("old begin: %v", err)
	}

	newerReady := make(chan error, 1)
	newerCommit := make(chan func(), 1)
	go func() {
		commit, rollback, err := r.begin("sp1", 7, "gh", 5, "delivery-v5")
		if err != nil {
			newerReady <- err
			return
		}
		newerCommit <- commit
		_ = rollback
		newerReady <- nil
	}()

	select {
	case err := <-newerReady:
		t.Fatalf("newer reservation completed while older delivery was still in-flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	oldRollback()
	select {
	case err := <-newerReady:
		if err != nil {
			t.Fatalf("newer begin after old rollback: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("newer reservation did not proceed after old rollback")
	}
	(<-newerCommit)()

	_, staleRollback, err := r.begin("sp1", 7, "gh", 4, "delivery-v4b")
	if err == nil {
		staleRollback()
		t.Fatal("older version began after newer version committed")
	}
}

// publishSubKey returns the current SignedSubKey JSON (rotating if needed); rotatedSubKey returns bytes
// only when the published key changes, so steady-state heartbeats carry none.
func TestPublishSubKey(t *testing.T) {
	a := &attacher{cfg: Config{NodeID: "node-1", SubKeys: subkey.NewNode(testSignKey(t), "node-1", 0)}}
	now := time.Now()
	b := a.publishSubKey(now)
	if b == nil {
		t.Fatal("publishSubKey returned nil with a holder present")
	}
	var sk subkey.SignedSubKey
	if err := json.Unmarshal(b, &sk); err != nil {
		t.Fatalf("published bytes are not a SignedSubKey: %v", err)
	}
	if sk.NodeID != "node-1" || len(sk.HPKEPub) != 32 {
		t.Fatalf("bad published sub-key: %+v", sk)
	}
	// No rotation since publish -> heartbeat carries nothing.
	if a.rotatedSubKey(now) != nil {
		t.Fatal("rotatedSubKey must be nil when the sub-key is unchanged")
	}
	// A node with no holder publishes nothing.
	empty := &attacher{cfg: Config{NodeID: "n"}}
	if empty.publishSubKey(now) != nil {
		t.Fatal("publishSubKey must be nil without a holder")
	}
}
