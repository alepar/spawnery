package cp

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/nodeauth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
	"spawnery/internal/pki"
)

// When the connection is authenticated (verified identity on the context), the CP records class/owner/id
// from the CERTIFICATE and ignores the self-asserted Register fields — even when the node lies.
func TestRegisterPrefersVerifiedIdentity(t *testing.T) {
	s, reg, _ := newTestServer(t)
	in := make(chan *nodev1.NodeMessage, 4)
	ctx := nodeauth.WithIdentity(context.Background(),
		pki.Principal{Kind: pki.KindNode, Role: pki.ClassSelfHosted, NodeID: "realnode", AccountID: "alice"})
	certChain := []byte("leaf-first-pem")
	ctx = nodeauth.WithCertChain(ctx, certChain)
	go s.runNode(ctx, &capSender{}, recvFromChan(in))

	// The node LIES: claims cloud class, owner bob, a different node id.
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{
		NodeId: "fakenode", MaxSpawns: 1, NodeClass: "cloud", NodeOwner: "bob", SignedSubkey: []byte("opaque-subkey"),
	}}}

	// The CP records the VERIFIED identity, not the lie.
	waitNodeClass(t, reg, "realnode", pki.ClassSelfHosted)
	n, ok := reg.Get("realnode")
	if !ok || n.Owner != "alice" {
		t.Fatalf("registered node = %+v (ok=%v), want owner=alice from the verified cert", n, ok)
	}
	// The self-asserted node id must never have been registered.
	if _, ok := reg.Get("fakenode"); ok {
		t.Fatal("self-asserted node_id must be ignored when authenticated")
	}
	entry, ok := s.nodeKeys.get("realnode")
	if !ok || entry.nodeID != "realnode" || entry.nodeClass != pki.ClassSelfHosted || entry.accountID != "alice" || string(entry.certChain) != string(certChain) {
		t.Fatalf("cached effective identity = %+v (ok=%v)", entry, ok)
	}
	close(in)
}

func TestAcceptedReconnectReplacesNodeIdentityCacheWithoutSubkey(t *testing.T) {
	s, reg, _ := newTestServer(t)
	oldIn := make(chan *nodev1.NodeMessage, 2)
	oldCtx := nodeauth.WithIdentity(context.Background(),
		pki.Principal{Kind: pki.KindNode, Role: pki.ClassSelfHosted, NodeID: "shared-node", AccountID: "alice"})
	oldCtx = nodeauth.WithCertChain(oldCtx, []byte("old-leaf-first-pem"))
	go s.runNode(oldCtx, &capSender{}, recvFromChan(oldIn))
	oldIn <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{
		NodeId: "shared-node", MaxSpawns: 1, SignedSubkey: []byte("old-subkey"),
	}}}
	waitNodeClass(t, reg, "shared-node", pki.ClassSelfHosted)
	if err := s.st.Owners().Upsert(context.Background(), store.Owner{ID: "owner", CreatedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	createActiveSpawn(t, s, "owner", "sp-reconnect", "shared-node")
	ctx := auth.WithOwner(context.Background(), "owner")

	// Model an accepted replacement after the old registry lease is no longer current while its
	// stream teardown is still pending. The node-key cache deliberately remains populated here.
	reg.Remove("shared-node")
	reg.Add(&registry.Node{ID: "shared-node", Class: pki.ClassCloud, Owner: "system", Max: 1, Free: 1})
	if _, err := s.GetSpawnNodeKey(ctx, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: "sp-reconnect"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("registry/cache token mismatch exposed stale metadata: err=%v", err)
	}
	reg.Remove("shared-node")
	newIn := make(chan *nodev1.NodeMessage, 2)
	newCtx := nodeauth.WithIdentity(context.Background(),
		pki.Principal{Kind: pki.KindNode, Role: pki.ClassCloud, NodeID: "shared-node", AccountID: "system"})
	newCtx = nodeauth.WithCertChain(newCtx, []byte("new-leaf-first-pem"))
	go s.runNode(newCtx, &capSender{}, recvFromChan(newIn))
	newIn <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{
		NodeId: "shared-node", MaxSpawns: 1, // replacement intentionally publishes no subkey
	}}}
	waitNodeClass(t, reg, "shared-node", pki.ClassCloud)

	if _, err := s.GetSpawnNodeKey(ctx, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: "sp-reconnect"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("replacement without subkey reused stale metadata: err=%v", err)
	}
	entry, ok := s.nodeKeys.get("shared-node")
	if !ok || entry.nodeClass != pki.ClassCloud || entry.accountID != "system" || string(entry.certChain) != "new-leaf-first-pem" || len(entry.subkey) != 0 {
		t.Fatalf("replacement cache = %+v (ok=%v), want only new incomplete tuple", entry, ok)
	}

	close(newIn)
	close(oldIn)
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := reg.Get("shared-node"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement node did not disconnect")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := s.nodeKeys.get("shared-node"); ok {
		t.Fatal("replacement disconnect retained its cache entry")
	}
}
