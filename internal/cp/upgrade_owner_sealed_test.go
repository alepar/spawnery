package cp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/journalkeys"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
)

// sealingSender is a fake NodeSender that responds to SealJournalKeyToOwnerRequest by calling back
// the server's upgradeWaiters with a pre-configured response.
type sealingSender struct {
	mu      sync.Mutex
	s       *Server
	entries []*nodev1.SealedJournalKey // the ciphertext to return
	errMsg  string                     // if non-empty, respond with this error
	lastReq *nodev1.SealJournalKeyToOwnerRequest
}

func (a *sealingSender) Send(m *nodev1.CPMessage) error {
	req := m.GetSealJournalKey()
	if req == nil {
		return nil // ignore other message types
	}
	a.mu.Lock()
	a.lastReq = req
	entries := a.entries
	errMsg := a.errMsg
	a.mu.Unlock()

	// Asynchronously deliver the response (mirrors the real node behaviour).
	go a.s.upgradeWaiters.deliver(&nodev1.SealJournalKeyToOwnerResponse{
		SpawnId:   req.SpawnId,
		RequestId: req.RequestId,
		Entries:   entries,
		Error:     errMsg,
	})
	return nil
}

// makeSealEnvelope builds a minimal seal.Envelope JSON sealed to pk (so assertOpenableByOwner passes).
func makeSealEnvelope(t *testing.T, pk []byte) []byte {
	t.Helper()
	env := &seal.Envelope{
		AtRest: seal.AtRestAAD{AccountID: "a", SecretID: "s", Version: 1},
		Recipients: []seal.RecipientSeal{{
			Recipient: seal.X25519PubKey(pk),
			Enc:       make([]byte, 32),
			CT:        make([]byte, 16),
		}},
		Nonce: make([]byte, 12),
		CT:    make([]byte, 1),
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("makeSealEnvelope: %v", err)
	}
	return b
}

// TestUpgradeToOwnerSealedStoresCiphertext: UpgradeToOwnerSealed stores each returned ciphertext
// via the journalKeys custody so subsequent GetJournalKeyCiphertext can serve it.
func TestUpgradeToOwnerSealedStoresCiphertext(t *testing.T) {
	s, reg, rt := newTestServer(t)
	ownerPub := make([]byte, 32) // 32-byte fake X25519 pubkey
	ownerPub[0] = 0x01

	ss := &sealingSender{s: s}
	ss.entries = []*nodev1.SealedJournalKey{{
		Mount:      "main",
		Ciphertext: makeSealEnvelope(t, ownerPub),
	}}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", ss)

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.UpgradeToOwnerSealed(ctx, connect.NewRequest(&cpv1.UpgradeToOwnerSealedRequest{
		SpawnId:            "sp1",
		OwnerDevicePubkeys: [][]byte{ownerPub},
	}))
	if err != nil {
		t.Fatalf("UpgradeToOwnerSealed: %v", err)
	}
	_ = resp

	// The ciphertext must now be stored.
	ct, err := s.journalKeys.Get(ctx, "sp1", journalkey.SecretID("main"))
	if err != nil {
		t.Fatalf("GetJournalKeyCiphertext after upgrade: %v", err)
	}
	if len(ct) == 0 {
		t.Fatal("stored ciphertext is empty")
	}
}

// TestUpgradeToOwnerSealedNodeErrorPropagated: a node error is surfaced as CodeInternal.
func TestUpgradeToOwnerSealedNodeErrorPropagated(t *testing.T) {
	s, reg, rt := newTestServer(t)
	ss := &sealingSender{s: s, errMsg: "mount not journaled"}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", ss)

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.UpgradeToOwnerSealed(ctx, connect.NewRequest(&cpv1.UpgradeToOwnerSealedRequest{
		SpawnId:            "sp1",
		OwnerDevicePubkeys: [][]byte{make([]byte, 32)},
	}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("node error: want Internal, got %v", err)
	}
}

// TestUpgradeToOwnerSealedAuthGuards: unauthenticated + foreign owner rejected.
func TestUpgradeToOwnerSealedAuthGuards(t *testing.T) {
	s, reg, rt := newTestServer(t)
	ss := &sealingSender{s: s}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", ss)

	// Unauthenticated.
	_, err := s.UpgradeToOwnerSealed(context.Background(), connect.NewRequest(&cpv1.UpgradeToOwnerSealedRequest{
		SpawnId:            "sp1",
		OwnerDevicePubkeys: [][]byte{make([]byte, 32)},
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauthenticated: want Unauthenticated, got %v", err)
	}

	// Foreign owner.
	bob := auth.WithOwner(context.Background(), "bob")
	_, err = s.UpgradeToOwnerSealed(bob, connect.NewRequest(&cpv1.UpgradeToOwnerSealedRequest{
		SpawnId:            "sp1",
		OwnerDevicePubkeys: [][]byte{make([]byte, 32)},
	}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign owner: want PermissionDenied, got %v", err)
	}
	_ = reg
	_ = rt
}

// TestUpgradeToOwnerSealedEmptyPubkeysRejected: empty device pubkeys fails with InvalidArgument.
func TestUpgradeToOwnerSealedEmptyPubkeysRejected(t *testing.T) {
	s, reg, rt := newTestServer(t)
	ss := &sealingSender{s: s}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", ss)

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.UpgradeToOwnerSealed(ctx, connect.NewRequest(&cpv1.UpgradeToOwnerSealedRequest{
		SpawnId:            "sp1",
		OwnerDevicePubkeys: nil,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty pubkeys: want InvalidArgument, got %v", err)
	}
	_ = reg
	_ = rt
}

// Verify journalkeys import is used.
var _ journalkeys.Store
