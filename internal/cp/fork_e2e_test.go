//go:build e2e

package cp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/journalkeys"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/node"
	"spawnery/internal/pki"
	"spawnery/internal/runtime"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage/journal"
)

func TestForkSameNodeE2E(t *testing.T) {
	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}

	fx := newForkE2EStack(t, rt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	create, err := fx.client.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "x"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	sourceID := create.Msg.SpawnId
	t.Cleanup(func() { fx.stopSpawn(sourceID) })
	forkWaitForStatus(ctx, t, fx.client, sourceID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	forkPromptEchoOnce(ctx, t, fx.client, sourceID, "before fork")
	sourceGen1 := forkFindGeneration(ctx, t, fx.client, sourceID)

	forkPoint := "fork-point:" + sourceID
	forkWriteFile(t, fx.mountPath(sourceID, "fork-point.txt"), forkPoint)

	forkResp, err := fx.client.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{
		SpawnId: sourceID,
		Name:    "same-node fork e2e child",
	}))
	if err != nil {
		t.Fatalf("ForkSpawn: %v", err)
	}
	forkID := forkResp.Msg.ForkSpawnId
	t.Cleanup(func() { fx.stopSpawn(forkID) })
	if forkResp.Msg.NodeId != fx.nodeID {
		t.Fatalf("ForkSpawn node_id=%q, want %q", forkResp.Msg.NodeId, fx.nodeID)
	}

	forkWaitForStatus(ctx, t, fx.client, sourceID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	forkWaitForStatus(ctx, t, fx.client, forkID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	forkPromptEchoOnce(ctx, t, fx.client, sourceID, "source after fork")
	forkPromptEchoOnce(ctx, t, fx.client, forkID, "fork after fork")
	forkAssertParent(ctx, t, fx.client, forkID, sourceID)
	forkAssertFile(t, fx.mountPath(sourceID, "fork-point.txt"), forkPoint)
	forkAssertFile(t, fx.mountPath(forkID, "fork-point.txt"), forkPoint)

	sourceOnly := "source-only:" + sourceID
	forkOnly := "fork-only:" + forkID
	forkWriteFile(t, fx.mountPath(sourceID, "post-fork", "source-only.txt"), sourceOnly)
	forkWriteFile(t, fx.mountPath(sourceID, "post-fork", "diverge.txt"), "source branch")
	forkWriteFile(t, fx.mountPath(forkID, "post-fork", "fork-only.txt"), forkOnly)
	forkWriteFile(t, fx.mountPath(forkID, "post-fork", "diverge.txt"), "fork branch")
	forkAssertDiverged(t, fx, sourceID, forkID, forkPoint, sourceOnly, forkOnly)

	if _, err := fx.client.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: sourceID})); err != nil {
		t.Fatalf("SuspendSpawn source: %v", err)
	}
	forkWaitForStatus(ctx, t, fx.client, sourceID, cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED)
	forkWaitForStatus(ctx, t, fx.client, forkID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	if _, err := fx.client.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: sourceID})); err != nil {
		t.Fatalf("ResumeSpawn source: %v", err)
	}
	forkWaitForStatus(ctx, t, fx.client, sourceID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	sourceGen2 := forkFindGeneration(ctx, t, fx.client, sourceID)
	if sourceGen2 <= sourceGen1 {
		t.Fatalf("source generation after resume=%d, want > %d", sourceGen2, sourceGen1)
	}
	forkGen1 := forkFindGeneration(ctx, t, fx.client, forkID)
	if forkGen1 != 1 {
		t.Fatalf("fork generation after source resume=%d, want 1", forkGen1)
	}
	forkAssertDiverged(t, fx, sourceID, forkID, forkPoint, sourceOnly, forkOnly)

	if _, err := fx.client.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: forkID})); err != nil {
		t.Fatalf("SuspendSpawn fork: %v", err)
	}
	forkWaitForStatus(ctx, t, fx.client, forkID, cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED)
	forkWaitForStatus(ctx, t, fx.client, sourceID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	if _, err := fx.client.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: forkID})); err != nil {
		t.Fatalf("ResumeSpawn fork: %v", err)
	}
	forkWaitForStatus(ctx, t, fx.client, forkID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	forkGen2 := forkFindGeneration(ctx, t, fx.client, forkID)
	if forkGen2 <= forkGen1 {
		t.Fatalf("fork generation after resume=%d, want > %d", forkGen2, forkGen1)
	}
	if got := forkFindGeneration(ctx, t, fx.client, sourceID); got != sourceGen2 {
		t.Fatalf("source generation changed while resuming fork: got %d want %d", got, sourceGen2)
	}
	forkAssertDiverged(t, fx, sourceID, forkID, forkPoint, sourceOnly, forkOnly)
}

func TestForkCrossNodeOwnerSealedE2E(t *testing.T) {
	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}

	targetID := "fork-e2e-n2"
	targetIdentity := newForkNodeIdentity(t, targetID, "alice", pki.ClassSelfHosted)
	fx := newForkE2EControlPlane(t)
	sourceID := "fork-e2e-n1"
	fx.startNode(t, rt, forkE2ENodeSpec{
		id:      sourceID,
		class:   pki.ClassSelfHosted,
		owner:   "alice",
		rootPEM: targetIdentity.root,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	create, err := fx.client.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "x"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	spawnID := create.Msg.SpawnId
	t.Cleanup(func() { fx.stopSpawn(spawnID) })
	forkWaitForStatus(ctx, t, fx.client, spawnID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	forkPromptEchoOnce(ctx, t, fx.client, spawnID, "cross before fork")
	sourceGen1 := forkFindGeneration(ctx, t, fx.client, spawnID)

	forkStoreSourceJournalKeyCiphertext(ctx, t, fx, spawnID)
	fx.startNode(t, rt, forkE2ENodeSpec{
		id:      targetID,
		class:   pki.ClassSelfHosted,
		owner:   "alice",
		subKeys: targetIdentity.subKeys,
		rootPEM: targetIdentity.root,
	})
	fx.server.nodeKeys.put(targetID, targetIdentity.signedSubKey, targetIdentity.certChain)

	forkPoint := "cross-fork-point:" + spawnID
	forkWriteFile(t, fx.mountPathOnNode(sourceID, spawnID, "fork-point.txt"), forkPoint)

	forkResp, err := fx.client.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{
		SpawnId:      spawnID,
		TargetNodeId: targetID,
		Name:         "cross-node fork e2e child",
	}))
	if err != nil {
		t.Fatalf("ForkSpawn cross-node: %v", err)
	}
	forkID := forkResp.Msg.ForkSpawnId
	t.Cleanup(func() { fx.stopSpawn(forkID) })
	if forkResp.Msg.NodeId != targetID {
		t.Fatalf("ForkSpawn node_id=%q, want %q", forkResp.Msg.NodeId, targetID)
	}
	forkDeliverOwnerSealedSecretToTarget(ctx, t, fx, forkID, targetID, targetIdentity.subKeys)

	forkWaitForStatus(ctx, t, fx.client, spawnID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	forkWaitForStatus(ctx, t, fx.client, forkID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	forkPromptEchoOnce(ctx, t, fx.client, spawnID, "cross source after fork")
	forkPromptEchoOnce(ctx, t, fx.client, forkID, "cross fork after fork")
	forkAssertParent(ctx, t, fx.client, forkID, spawnID)
	forkAssertFile(t, fx.mountPathOnNode(sourceID, spawnID, "fork-point.txt"), forkPoint)
	forkAssertFile(t, fx.mountPathOnNode(targetID, forkID, "fork-point.txt"), forkPoint)

	sourceOnly := "cross-source-only:" + spawnID
	forkOnly := "cross-fork-only:" + forkID
	forkWriteFile(t, fx.mountPathOnNode(sourceID, spawnID, "post-fork", "source-only.txt"), sourceOnly)
	forkWriteFile(t, fx.mountPathOnNode(sourceID, spawnID, "post-fork", "diverge.txt"), "source branch")
	forkWriteFile(t, fx.mountPathOnNode(targetID, forkID, "post-fork", "fork-only.txt"), forkOnly)
	forkWriteFile(t, fx.mountPathOnNode(targetID, forkID, "post-fork", "diverge.txt"), "fork branch")
	forkAssertDivergedOnNodes(t, fx, sourceID, spawnID, targetID, forkID, forkPoint, sourceOnly, forkOnly)

	if _, err := fx.client.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: spawnID})); err != nil {
		t.Fatalf("SuspendSpawn source: %v", err)
	}
	forkWaitForStatus(ctx, t, fx.client, spawnID, cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED)
	forkWaitForStatus(ctx, t, fx.client, forkID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	if _, err := fx.client.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: spawnID})); err != nil {
		t.Fatalf("ResumeSpawn source: %v", err)
	}
	forkWaitForStatus(ctx, t, fx.client, spawnID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	sourceGen2 := forkFindGeneration(ctx, t, fx.client, spawnID)
	if sourceGen2 <= sourceGen1 {
		t.Fatalf("source generation after resume=%d, want > %d", sourceGen2, sourceGen1)
	}
	forkGen1 := forkFindGeneration(ctx, t, fx.client, forkID)
	if forkGen1 != 1 {
		t.Fatalf("fork generation after source resume=%d, want 1", forkGen1)
	}
	forkAssertDivergedOnNodes(t, fx, sourceID, spawnID, targetID, forkID, forkPoint, sourceOnly, forkOnly)

	if _, err := fx.client.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: forkID})); err != nil {
		t.Fatalf("SuspendSpawn fork: %v", err)
	}
	forkWaitForStatus(ctx, t, fx.client, forkID, cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED)
	forkWaitForStatus(ctx, t, fx.client, spawnID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	if _, err := fx.client.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: forkID})); err != nil {
		t.Fatalf("ResumeSpawn fork: %v", err)
	}
	forkWaitForStatus(ctx, t, fx.client, forkID, cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE)
	forkGen2 := forkFindGeneration(ctx, t, fx.client, forkID)
	if forkGen2 <= forkGen1 {
		t.Fatalf("fork generation after resume=%d, want > %d", forkGen2, forkGen1)
	}
	if got := forkFindGeneration(ctx, t, fx.client, spawnID); got != sourceGen2 {
		t.Fatalf("source generation changed while resuming fork: got %d want %d", got, sourceGen2)
	}
	forkAssertDivergedOnNodes(t, fx, sourceID, spawnID, targetID, forkID, forkPoint, sourceOnly, forkOnly)
}

type forkE2EStack struct {
	client   cpv1connect.SpawnServiceClient
	server   *Server
	cpURL    string
	dataDirs map[string]string
	nodeID   string
}

func newForkE2EStack(t *testing.T, rt runtime.ContainerRuntime) *forkE2EStack {
	t.Helper()
	fx := newForkE2EControlPlane(t)
	fx.startNode(t, rt, forkE2ENodeSpec{id: "fork-e2e-n1"})
	fx.nodeID = "fork-e2e-n1"
	return fx
}

func newForkE2EControlPlane(t *testing.T) *forkE2EStack {
	t.Helper()
	reg := registry.New()
	rtr := router.New()
	sched := scheduler.New(reg, rtr, 60*time.Second)
	telPath := filepath.Join(t.TempDir(), "events.jsonl")
	tel, err := telemetry.NewJSONLSink(telPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Close() })

	appRef, err := filepath.Abs("../../examples/secret-app")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    fmt.Sprintf("file:fork_same_node_e2e_%d?mode=memory&cache=shared&_pragma=foreign_keys(1)", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]AppSeed{{ID: "secret-app", Ref: appRef, Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(reg, rtr, sched, st, tel)
	srv.forkFootprintEstimator = staticForkFootprint(0)
	authn := auth.NewVerifier(auth.VerifierConfig{DevTokens: map[string]string{"dev-token": "alice"}, DevMode: true})

	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(srv))
	mux.Handle(cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(authn.Interceptor())))
	cpSrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	cpSrv.Start()
	t.Cleanup(cpSrv.Close)

	client := cpv1connect.NewSpawnServiceClient(forkH2CClient(), cpSrv.URL, connect.WithGRPC(),
		connect.WithInterceptors(forkBearer("dev-token")))
	return &forkE2EStack{client: client, server: srv, cpURL: cpSrv.URL, dataDirs: map[string]string{}}
}

type forkE2ENodeSpec struct {
	id      string
	class   string
	owner   string
	subKeys *subkey.Node
	rootPEM []byte
}

type forkNodeIdentity struct {
	root         []byte
	certChain    []byte
	signedSubKey []byte
	subKeys      *subkey.Node
}

func newForkNodeIdentity(t *testing.T, nodeID, account, class string) forkNodeIdentity {
	t.Helper()
	root, err := pki.NewRootCA("fork-e2e-root")
	if err != nil {
		t.Fatalf("NewRootCA: %v", err)
	}
	inter, err := root.NewIntermediate(class)
	if err != nil {
		t.Fatalf("NewIntermediate: %v", err)
	}
	issued, err := inter.IssueNode(nodeID, account, class, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	holder := subkey.NewNode(issued.Key, nodeID, time.Hour)
	published, err := holder.Rotate(time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Rotate subkey: %v", err)
	}
	signed, err := json.Marshal(published)
	if err != nil {
		t.Fatalf("marshal subkey: %v", err)
	}
	leaf := pki.MarshalCertPEM(issued.Cert)
	chain := pki.MarshalCertPEM(inter.Cert)
	return forkNodeIdentity{
		root:         pki.MarshalCertPEM(root.Cert),
		certChain:    append(append([]byte{}, leaf...), chain...),
		signedSubKey: signed,
		subKeys:      holder,
	}
}

func forkStoreSourceJournalKeyCiphertext(ctx context.Context, t *testing.T, fx *forkE2EStack, spawnID string) {
	t.Helper()
	mnemonic, err := seal.NewMnemonic()
	if err != nil {
		t.Fatalf("NewMnemonic: %v", err)
	}
	dev, err := seal.DeviceFromMnemonic(mnemonic, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic: %v", err)
	}
	fx.server.ownerDevices = journalkeys.StaticRegistry{
		"alice": []seal.X25519PubKey{dev.X25519PubKey()},
	}
	ciphertext := ownerSealedCiphertext(t, "source-repo-password:"+spawnID, "alice", "main", dev)
	if _, err := fx.client.PutJournalKeyCiphertext(ctx, connect.NewRequest(&cpv1.PutJournalKeyCiphertextRequest{
		SpawnId: spawnID,
		Entries: []*cpv1.JournalKeyCiphertext{{
			Mount:      "main",
			Ciphertext: ciphertext,
		}},
	})); err != nil {
		t.Fatalf("PutJournalKeyCiphertext source: %v", err)
	}
}

func forkDeliverOwnerSealedSecretToTarget(ctx context.Context, t *testing.T, fx *forkE2EStack, forkID, targetID string, targetSubKeys *subkey.Node) {
	t.Helper()
	if !fx.server.deliveryPending.isPending(forkID) {
		t.Fatal("cross-node fork should be delivery pending before owner-sealed delivery")
	}
	key, err := fx.client.GetSpawnNodeKey(ctx, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: forkID}))
	if err != nil {
		t.Fatalf("GetSpawnNodeKey fork: %v", err)
	}
	if key.Msg.Generation != 1 {
		t.Fatalf("fork delivery generation=%d, want 1", key.Msg.Generation)
	}
	if len(key.Msg.SignedSubkey) == 0 || len(key.Msg.NodeCertChain) == 0 {
		t.Fatalf("fork delivery key material missing: subkey=%d cert=%d", len(key.Msg.SignedSubkey), len(key.Msg.NodeCertChain))
	}
	now := time.Now()
	current, ok := targetSubKeys.Current(now)
	if !ok || current.NodeID != targetID {
		t.Fatalf("target subkey current=%+v ok=%v, want node %s", current, ok, targetID)
	}
	sealed, err := seal.SealPlaintextToNode([]byte("owner-sealed-probe:"+forkID), current.HPKEPub, seal.InFlightAAD{
		SpawnID:    forkID,
		Generation: key.Msg.Generation,
		NodeID:     targetID,
		NotAfter:   current.NotAfter,
	})
	if err != nil {
		t.Fatalf("SealPlaintextToNode fork owner-sealed secret: %v", err)
	}
	payload, err := json.Marshal(sealed)
	if err != nil {
		t.Fatalf("marshal sealed fork owner-sealed secret: %v", err)
	}
	if _, err := fx.client.DeliverSecrets(ctx, connect.NewRequest(&cpv1.DeliverSecretsRequest{
		SpawnId: forkID,
		Secrets: []*cpv1.SealedSecret{{
			TargetPath: "delivery/probe",
			Sealed:     payload,
			SecretId:   "delivery-probe",
		}},
	})); err != nil {
		t.Fatalf("DeliverSecrets fork owner-sealed secret: %v", err)
	}
}

func (fx *forkE2EStack) startNode(t *testing.T, rt runtime.ContainerRuntime, spec forkE2ENodeSpec) {
	t.Helper()
	if spec.id == "" {
		t.Fatal("fork e2e node id is required")
	}
	dataDir := t.TempDir()
	jm := forkBuildJournalForTest(t, dataDir)
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage: "spawnery/stubagent:dev", SidecarImage: "spawnery/sidecar:dev",
		OpenRouterKey: "unused", DataRoot: dataDir, NodeID: spec.id, NodeClass: spec.class,
	})
	mgr.SetJournal(jm, filepath.Join(dataDir, "journal", "state"))

	nodeCtx, stopNode := context.WithCancel(context.Background())
	t.Cleanup(stopNode)
	go node.Run(nodeCtx, mgr, forkH2CClient(), node.Config{
		NodeID: spec.id, CPURL: fx.cpURL, MaxSpawns: 4, AgentImage: "spawnery/stubagent:dev",
		NodeClass: spec.class, NodeOwner: spec.owner, SubKeys: spec.subKeys, NodeRootPEM: spec.rootPEM,
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := fx.server.reg.Get(spec.id); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %s never registered with CP", spec.id)
		}
		time.Sleep(50 * time.Millisecond)
	}
	fx.dataDirs[spec.id] = dataDir
}

func (fx *forkE2EStack) mountPath(spawnID string, elems ...string) string {
	return fx.mountPathOnNode(fx.nodeID, spawnID, elems...)
}

func (fx *forkE2EStack) mountPathOnNode(nodeID, spawnID string, elems ...string) string {
	dataDir := fx.dataDirs[nodeID]
	if dataDir == "" {
		panic("unknown fork e2e node data dir: " + nodeID)
	}
	parts := append([]string{dataDir, spawnID, "main"}, elems...)
	return filepath.Join(parts...)
}

func (fx *forkE2EStack) stopSpawn(spawnID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = fx.client.StopSpawn(ctx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: spawnID}))
}

func forkBuildJournalForTest(t *testing.T, dataRoot string) journal.JournalManager {
	t.Helper()

	s3cfg, ok := forkGarageS3Config(t)
	if !ok {
		t.Fatalf("Garage S3 credentials not available; start dev Garage with `just garage`, or set JOURNAL_S3_*")
	}
	root := filepath.Join(dataRoot, "journal")
	backend, err := journal.NewBackend(journal.BackendConfig{
		Kind: journal.BackendS3,
		S3:   s3cfg,
	})
	if err != nil {
		t.Fatalf("journal S3 backend: %v", err)
	}
	keyfile := filepath.Join(root, "node.key")
	if err := journal.GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatalf("journal node key: %v", err)
	}
	custody, err := journal.NewNodeLocalCustody(keyfile, filepath.Join(root, "sealed"))
	if err != nil {
		t.Fatalf("journal custody: %v", err)
	}
	jm, err := journal.NewManager(journal.Config{
		RepoRoot:    filepath.Join(root, "repos"),
		Backend:     backend,
		Custody:     custody,
		OwnerSealed: journal.NewOwnerSealedCustody(),
	})
	if err != nil {
		t.Fatalf("journal manager: %v", err)
	}
	return jm
}

func forkGarageS3Config(t *testing.T) (journal.S3Config, bool) {
	t.Helper()

	if ep := os.Getenv("JOURNAL_S3_ENDPOINT"); ep != "" {
		return journal.S3Config{
			Endpoint:        ep,
			Bucket:          forkEnvOr("JOURNAL_S3_BUCKET", "spawnery-journal"),
			AccessKeyID:     os.Getenv("JOURNAL_S3_ACCESS_KEY"),
			SecretAccessKey: os.Getenv("JOURNAL_S3_SECRET_KEY"),
			Region:          forkEnvOr("JOURNAL_S3_REGION", "garage"),
			DisableTLS:      os.Getenv("JOURNAL_S3_DISABLE_TLS") == "true",
		}, true
	}

	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "garage", "dev-creds.env"))
	if err != nil {
		return journal.S3Config{}, false
	}
	kv := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		kv[k] = v
	}
	if kv["JOURNAL_S3_ENDPOINT"] == "" {
		return journal.S3Config{}, false
	}
	return journal.S3Config{
		Endpoint:        kv["JOURNAL_S3_ENDPOINT"],
		Bucket:          forkKVOr(kv, "JOURNAL_S3_BUCKET", "spawnery-journal"),
		AccessKeyID:     kv["JOURNAL_S3_ACCESS_KEY"],
		SecretAccessKey: kv["JOURNAL_S3_SECRET_KEY"],
		Region:          forkKVOr(kv, "JOURNAL_S3_REGION", "garage"),
		DisableTLS:      kv["JOURNAL_S3_DISABLE_TLS"] == "true",
	}, true
}

func forkEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func forkKVOr(m map[string]string, key, def string) string {
	if v := m[key]; v != "" {
		return v
	}
	return def
}

func forkWaitForStatus(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, spawnID string, want cpv1.SpawnStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		summary := forkFindSummary(ctx, t, cl, spawnID)
		if summary.Status == want {
			return
		}
		switch summary.Status {
		case cpv1.SpawnStatus_SPAWN_STATUS_ERROR, cpv1.SpawnStatus_SPAWN_STATUS_DELETED,
			cpv1.SpawnStatus_SPAWN_STATUS_UNREACHABLE:
			t.Fatalf("spawn %s reached terminal status %v before %v", spawnID, summary.Status, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn %s did not reach %v within 2m; last status=%v", spawnID, want, summary.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func forkFindGeneration(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, spawnID string) uint64 {
	t.Helper()
	return forkFindSummary(ctx, t, cl, spawnID).GetGeneration()
}

func forkAssertParent(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, forkID, sourceID string) {
	t.Helper()
	summary := forkFindSummary(ctx, t, cl, forkID)
	if summary.GetParentSpawnId() != sourceID {
		t.Fatalf("fork parent_spawn_id=%q, want %q", summary.GetParentSpawnId(), sourceID)
	}
	if summary.GetForkedAt() == 0 {
		t.Fatalf("fork forked_at=0, want non-zero")
	}
}

func forkFindSummary(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, spawnID string) *cpv1.SpawnSummary {
	t.Helper()
	ls, err := cl.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatalf("ListSpawns: %v", err)
	}
	for _, sp := range ls.Msg.Spawns {
		if sp.SpawnId == spawnID {
			return sp
		}
	}
	t.Fatalf("spawn %s not found in ListSpawns", spawnID)
	return nil
}

func forkPromptEchoOnce(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, spawnID, text string) {
	t.Helper()

	stream := cl.Session(ctx)
	defer stream.CloseRequest()
	if err := stream.Send(&cpv1.Frame{SpawnId: spawnID}); err != nil {
		t.Fatalf("prompt bind %q: %v", text, err)
	}
	pr, pw := io.Pipe()
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			_, _ = pw.Write(f.Data)
		}
	}()

	b, _ := json.Marshal(map[string]any{"kind": "prompt", "text": text})
	if err := stream.Send(&cpv1.Frame{SpawnId: spawnID, Data: append(b, '\n')}); err != nil {
		t.Fatalf("prompt send %q: %v", text, err)
	}

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var got strings.Builder
	expected := "ECHO: " + text
	seenExpected := false
	for sc.Scan() {
		var fr struct {
			Kind  string `json:"kind"`
			Text  string `json:"text"`
			State string `json:"state"`
		}
		if json.Unmarshal(sc.Bytes(), &fr) != nil {
			continue
		}
		if fr.Kind == "agent" {
			got.WriteString(fr.Text)
			if strings.Contains(got.String(), expected) {
				seenExpected = true
			}
		}
		if fr.Kind == "turn" && fr.State == "idle" && seenExpected {
			break
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("prompt read %q: %v", text, err)
	}
	if !seenExpected {
		t.Fatalf("prompt want ECHO %q, got %q", text, got.String())
	}
}

func forkWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func forkAssertDiverged(t *testing.T, fx *forkE2EStack, sourceID, forkID, forkPoint, sourceOnly, forkOnly string) {
	t.Helper()
	forkAssertDivergedOnNodes(t, fx, fx.nodeID, sourceID, fx.nodeID, forkID, forkPoint, sourceOnly, forkOnly)
}

func forkAssertDivergedOnNodes(t *testing.T, fx *forkE2EStack, sourceNodeID, sourceID, forkNodeID, forkID, forkPoint, sourceOnly, forkOnly string) {
	t.Helper()
	forkAssertFile(t, fx.mountPathOnNode(sourceNodeID, sourceID, "fork-point.txt"), forkPoint)
	forkAssertFile(t, fx.mountPathOnNode(forkNodeID, forkID, "fork-point.txt"), forkPoint)
	forkAssertFile(t, fx.mountPathOnNode(sourceNodeID, sourceID, "post-fork", "source-only.txt"), sourceOnly)
	forkAssertFile(t, fx.mountPathOnNode(sourceNodeID, sourceID, "post-fork", "diverge.txt"), "source branch")
	forkAssertMissing(t, fx.mountPathOnNode(sourceNodeID, sourceID, "post-fork", "fork-only.txt"))
	forkAssertFile(t, fx.mountPathOnNode(forkNodeID, forkID, "post-fork", "fork-only.txt"), forkOnly)
	forkAssertFile(t, fx.mountPathOnNode(forkNodeID, forkID, "post-fork", "diverge.txt"), "fork branch")
	forkAssertMissing(t, fx.mountPathOnNode(forkNodeID, forkID, "post-fork", "source-only.txt"))
}

func forkAssertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s content=%q, want %q", path, string(got), want)
	}
}

func forkAssertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s exists, want missing", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
}

func forkH2CClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

func forkBearer(token string) connect.Interceptor { return forkBearerIntc{token: token} }

type forkBearerIntc struct{ token string }

func (b forkBearerIntc) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.token)
		return next(ctx, req)
	}
}

func (b forkBearerIntc) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return conn
	}
}

func (b forkBearerIntc) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
