package journalkeys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
)

func TestMemStorePutGetDelete(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	sid := journalkey.SecretID("work")
	ct := []byte("opaque-owner-sealed-ciphertext")

	if _, err := s.Get(ctx, "spawn-1", sid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Put = %v, want ErrNotFound", err)
	}
	if err := s.Put(ctx, "spawn-1", sid, ct); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "spawn-1", sid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, ct) {
		t.Fatalf("Get = %q, want %q", got, ct)
	}

	// Get returns a copy: mutating it must not corrupt the store.
	got[0] = 'X'
	again, _ := s.Get(ctx, "spawn-1", sid)
	if !bytes.Equal(again, ct) {
		t.Fatal("Get must return a defensive copy")
	}

	// Delete drops all of a spawn's keys.
	if err := s.Delete(ctx, "spawn-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "spawn-1", sid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	// Delete of an absent spawn is success.
	if err := s.Delete(ctx, "ghost"); err != nil {
		t.Fatalf("Delete absent: %v", err)
	}
	// Empty ids rejected.
	if err := s.Put(ctx, "", sid, ct); err == nil {
		t.Fatal("Put with empty spawnID must error")
	}
}

// TestMemStoreHoldsOnlyCiphertext is a behavioural assertion of the "CP stores
// ONLY ciphertext" contract: what goes in is the opaque owner-sealed envelope,
// and the store never needs (or is given) any key to read it back.
func TestMemStoreHoldsOnlyCiphertext(t *testing.T) {
	ctx := context.Background()
	// A real owner-sealed envelope's serialized bytes.
	m, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(m, "")
	env, err := journalkey.SealToOwner("repo-pw",
		[]seal.X25519PubKey{dev.X25519PubKey()},
		seal.AtRestAAD{AccountID: "o", SecretID: journalkey.SecretID("work"), Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := mustJSON(t, env)

	s := NewMemStore()
	if err := s.Put(ctx, "spawn-c", journalkey.SecretID("work"), ciphertext); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "spawn-c", journalkey.SecretID("work"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, ciphertext) {
		t.Fatal("stored ciphertext mismatch")
	}
	// The plaintext password never appears in what the CP holds.
	if bytes.Contains(got, []byte("repo-pw")) {
		t.Fatal("plaintext password leaked into CP-stored ciphertext")
	}
}

func TestOwnerDeviceRegistrySeam(t *testing.T) {
	ctx := context.Background()

	// Default: unwired -> fail closed.
	if _, err := (UnwiredRegistry{}).Devices(ctx, "owner-1"); !errors.Is(err, ErrRegistryNotWired) {
		t.Fatalf("UnwiredRegistry = %v, want ErrRegistryNotWired", err)
	}

	// Static (test/single-owner) registry.
	m, _ := seal.NewMnemonic()
	dev, _ := seal.DeviceFromMnemonic(m, "")
	reg := StaticRegistry{"owner-1": {dev.X25519PubKey()}}
	got, err := reg.Devices(ctx, "owner-1")
	if err != nil || len(got) != 1 {
		t.Fatalf("StaticRegistry.Devices = %v, %v; want 1 device", got, err)
	}
	// Unknown owner fails closed (never seals to an empty recipient set).
	if _, err := reg.Devices(ctx, "owner-unknown"); !errors.Is(err, ErrRegistryNotWired) {
		t.Fatalf("unknown owner = %v, want ErrRegistryNotWired", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
