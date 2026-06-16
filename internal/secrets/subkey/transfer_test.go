package subkey_test

import (
	"bytes"
	"testing"
	"time"

	"spawnery/internal/pki"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
)

func TestSealTransferKeyForVerifiedNode(t *testing.T) {
	fx := issue(t, "node-2", "alice", pki.ClassSelfHosted)
	now := time.Now().UTC()
	holder := subkey.NewNode(fx.key, "node-2", time.Hour)
	published, err := holder.Rotate(now)
	if err != nil {
		t.Fatal(err)
	}
	sealed, aad, err := subkey.SealTransferKeyForNode(
		[]byte("01234567890123456789012345678901"),
		fx.leaf,
		fx.chain,
		fx.root,
		published,
		subkey.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: "alice"},
		subkey.AllowAll{},
		seal.InFlightAAD{SpawnID: "sp-fork", Generation: 1, DeliveryID: "ts-1"},
		now,
	)
	if err != nil {
		t.Fatalf("SealTransferKeyForNode: %v", err)
	}
	opened, err := holder.OpenDelivered(sealed, seal.InFlightAAD{SpawnID: "sp-fork", Generation: 1, DeliveryID: "ts-1"}, now)
	if err != nil {
		t.Fatalf("OpenDelivered: %v", err)
	}
	if !bytes.Equal(opened, []byte("01234567890123456789012345678901")) || aad.NodeID != "node-2" {
		t.Fatalf("opened=%q aad=%+v", opened, aad)
	}
}

func TestSealTransferKeyForNodeRejectsWrongRoot(t *testing.T) {
	fx := issue(t, "node-2", "alice", pki.ClassSelfHosted)
	other := issue(t, "node-other", "alice", pki.ClassSelfHosted)
	now := time.Now().UTC()
	published, _ := subkey.Sign(fx.key, "node-2", bytes.Repeat([]byte{1}, 32), now, now.Add(time.Hour))
	_, _, err := subkey.SealTransferKeyForNode(
		[]byte("k"),
		fx.leaf,
		fx.chain,
		other.root,
		published,
		subkey.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: "alice"},
		subkey.AllowAll{},
		seal.InFlightAAD{SpawnID: "sp-fork", Generation: 1, DeliveryID: "ts-1"},
		now,
	)
	if err == nil {
		t.Fatal("wrong root must reject")
	}
}

func TestSealTransferKeyForNodeRequiresPinnedRoot(t *testing.T) {
	fx := issue(t, "node-2", "alice", pki.ClassSelfHosted)
	now := time.Now().UTC()
	published, _ := subkey.Sign(fx.key, "node-2", bytes.Repeat([]byte{1}, 32), now, now.Add(time.Hour))
	_, _, err := subkey.SealTransferKeyForNode(
		[]byte("k"),
		fx.leaf,
		fx.chain,
		nil,
		published,
		subkey.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: "alice"},
		subkey.AllowAll{},
		seal.InFlightAAD{SpawnID: "sp-fork", Generation: 1, DeliveryID: "ts-1"},
		now,
	)
	if err == nil {
		t.Fatal("nil root must reject")
	}
}
