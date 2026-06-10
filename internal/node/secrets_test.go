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
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
	"spawnery/internal/spawnlet"
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
func sealSecret(t *testing.T, holder *subkey.Node, spawnID string, gen uint64, target, secretID string, payload []byte) *nodev1.SealedSecret {
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
	aad := seal.InFlightAAD{SpawnID: spawnID, Generation: gen, NodeID: signed.NodeID, NotAfter: signed.NotAfter}
	ns, err := seal.ReSealToNode(env, devPriv, signed.HPKEPub, aad)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(ns)
	if err != nil {
		t.Fatal(err)
	}
	return &nodev1.SealedSecret{TargetPath: target, Sealed: b, SecretId: secretID}
}

// Full round trip: seal with seal+subkey -> the wire SecretDelivery the CP relays -> node unseals ->
// plaintext lands in the spawn's tmpfs secrets dir at the declared path, mode 0600, exact content.
func TestSecretDeliveryUnsealsAndWritesTmpfs(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(7)
	a, holder, secretsRoot := secretTestRig(t, nodeID, spawnID, gen)
	want := []byte("ghp_supersecrettoken\n")
	sec := sealSecret(t, holder, spawnID, gen, "gh/hosts.yml", "gh", want)

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
	sec := sealSecret(t, holder, spawnID, 6, "gh/hosts.yml", "gh", []byte("stale"))

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
	sec := sealSecret(t, holder, spawnID, 99, "gh/hosts.yml", "gh", []byte("nope"))

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
