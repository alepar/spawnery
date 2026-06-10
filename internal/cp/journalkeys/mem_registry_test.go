package journalkeys

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"spawnery/internal/secrets/seal"
)

func TestMemDeviceRegistryEnrollAndFailClosed(t *testing.T) {
	ctx := context.Background()
	r := NewMemDeviceRegistry()

	// Fail closed for an un-enrolled owner (never seal to an empty recipient set).
	if _, err := r.Devices(ctx, "alice"); !errors.Is(err, ErrRegistryNotWired) {
		t.Fatalf("un-enrolled owner = %v, want ErrRegistryNotWired", err)
	}

	m, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(m, "")
	r.Enroll("alice", dev.X25519PubKey())

	got, err := r.Devices(ctx, "alice")
	if err != nil || len(got) != 1 {
		t.Fatalf("Devices = %v, %v; want 1 device", got, err)
	}
	if !bytes.Equal(got[0], dev.X25519PubKey()) {
		t.Fatal("enrolled device pubkey mismatch")
	}

	// Returned slice is a copy: mutating it must not corrupt the registry.
	got[0][0] ^= 0xFF
	again, _ := r.Devices(ctx, "alice")
	if !bytes.Equal(again[0], dev.X25519PubKey()) {
		t.Fatal("Devices must return a defensive copy")
	}

	// MemDeviceRegistry satisfies the seam (so it can REPLACE UnwiredRegistry as the CP default).
	var _ OwnerDeviceRegistry = r
}
