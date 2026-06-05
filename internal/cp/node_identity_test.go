package cp

import (
	"context"
	"testing"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/nodeauth"
	"spawnery/internal/pki"
)

// When the connection is authenticated (verified identity on the context), the CP records class/owner/id
// from the CERTIFICATE and ignores the self-asserted Register fields — even when the node lies.
func TestRegisterPrefersVerifiedIdentity(t *testing.T) {
	s, reg, _ := newTestServer(t)
	in := make(chan *nodev1.NodeMessage, 4)
	ctx := nodeauth.WithIdentity(context.Background(),
		pki.Identity{NodeID: "realnode", AccountID: "alice", Class: pki.ClassSelfHosted})
	go s.runNode(ctx, &capSender{}, recvFromChan(in))

	// The node LIES: claims cloud class, owner bob, a different node id.
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{
		NodeId: "fakenode", MaxSpawns: 1, NodeClass: "cloud", NodeOwner: "bob",
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
	close(in)
}
