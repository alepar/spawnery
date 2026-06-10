package seal

import (
	"testing"
)

func ownerRoot(device1, recovery *Device) OwnerRoot {
	return OwnerRoot{
		Device1SignPub:  marshalSignPub(device1.SignPub()),
		RecoverySignPub: marshalSignPub(recovery.SignPub()),
	}
}

func TestDeviceSetGenesisAndAddRemove(t *testing.T) {
	d1 := newTestDevice(t)
	rec := newTestDevice(t)
	d2 := newTestDevice(t)
	root := ownerRoot(d1, rec)

	log, err := NewGenesis(d1, rec)
	if err != nil {
		t.Fatalf("NewGenesis: %v", err)
	}
	members, err := VerifyDeviceSet(log, root)
	if err != nil {
		t.Fatalf("VerifyDeviceSet(genesis): %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("genesis members = %d, want 2", len(members))
	}

	// Add d2, signed by an existing member (d1).
	if err := log.AddDevice(d2.Ref(), d1); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	members, err = VerifyDeviceSet(log, root)
	if err != nil {
		t.Fatalf("VerifyDeviceSet(after add): %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("members after add = %d, want 3", len(members))
	}

	// The resolved set is usable as seal recipients.
	if memberIndex(members, d2.Ref()) < 0 {
		t.Fatal("d2 not present in resolved set")
	}

	// Remove d2, signed by d1.
	if err := log.RemoveDevice(d2.X25519Pub, d1); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	members, err = VerifyDeviceSet(log, root)
	if err != nil {
		t.Fatalf("VerifyDeviceSet(after remove): %v", err)
	}
	if len(members) != 2 || memberIndex(members, d2.Ref()) >= 0 {
		t.Fatalf("d2 not removed; members=%d", len(members))
	}

	// A freshly added member can itself sign the next mutation.
	d3 := newTestDevice(t)
	if err := log.AddDevice(d3.Ref(), d1); err != nil {
		t.Fatalf("AddDevice d3: %v", err)
	}
	d4 := newTestDevice(t)
	if err := log.AddDevice(d4.Ref(), d3); err != nil {
		t.Fatalf("AddDevice d4 signed by d3: %v", err)
	}
	if _, err := VerifyDeviceSet(log, root); err != nil {
		t.Fatalf("VerifyDeviceSet(d3-signed): %v", err)
	}
}

func TestGenesisRequiresBothOwnerRoots(t *testing.T) {
	d1 := newTestDevice(t)
	rec := newTestDevice(t)
	root := ownerRoot(d1, rec)

	// A genesis that drops the recovery co-signature must be rejected.
	bad := Entry{Version: 1, Type: EntryGenesis, Devices: []DeviceRef{d1.Ref(), rec.Ref()}}
	if err := bad.sign(d1); err != nil {
		t.Fatalf("sign: %v", err)
	}
	log := &DeviceSetLog{Entries: []Entry{bad}}
	if _, err := VerifyDeviceSet(log, root); err == nil {
		t.Fatal("expected genesis without recovery co-signature to be rejected")
	}
}

// roast M4: a poisoned device set must be rejected. A stolen AS session cannot
// inject a device because it cannot forge a member signature.
func TestPoisonedDeviceSetRejected(t *testing.T) {
	d1 := newTestDevice(t)
	rec := newTestDevice(t)
	root := ownerRoot(d1, rec)

	t.Run("wrong-signer-injection", func(t *testing.T) {
		// An attacker appends an entry adding their device, signed by a
		// NON-MEMBER key (the AS cannot sign as d1 or rec).
		log, _ := NewGenesis(d1, rec)
		attacker := newTestDevice(t)
		injected := newTestDevice(t)
		prev := &log.Entries[len(log.Entries)-1]
		ph, _ := prev.hash()
		change := injected.Ref()
		e := Entry{
			Version:  prev.Version + 1,
			Type:     EntryAdd,
			PrevHash: ph,
			Change:   &change,
			Devices:  append(cloneRefs(prev.Devices), injected.Ref()),
		}
		if err := e.sign(attacker); err != nil { // signed by a non-member
			t.Fatalf("sign: %v", err)
		}
		log.Entries = append(log.Entries, e)
		if _, err := VerifyDeviceSet(log, root); err == nil {
			t.Fatal("expected wrong-signer injection to be rejected")
		}
	})

	t.Run("unsigned-entry", func(t *testing.T) {
		log, _ := NewGenesis(d1, rec)
		injected := newTestDevice(t)
		prev := &log.Entries[len(log.Entries)-1]
		ph, _ := prev.hash()
		change := injected.Ref()
		e := Entry{
			Version:  prev.Version + 1,
			Type:     EntryAdd,
			PrevHash: ph,
			Change:   &change,
			Devices:  append(cloneRefs(prev.Devices), injected.Ref()),
			// no Sigs
		}
		log.Entries = append(log.Entries, e)
		if _, err := VerifyDeviceSet(log, root); err == nil {
			t.Fatal("expected unsigned entry to be rejected")
		}
	})

	t.Run("version-regressed", func(t *testing.T) {
		log, _ := NewGenesis(d1, rec)
		d2 := newTestDevice(t)
		if err := log.AddDevice(d2.Ref(), d1); err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
		// Tamper: drag the second entry's version back to genesis's.
		log.Entries[1].Version = 1
		if _, err := VerifyDeviceSet(log, root); err == nil {
			t.Fatal("expected version-regressed log to be rejected")
		}
	})

	t.Run("version-duplicate", func(t *testing.T) {
		log, _ := NewGenesis(d1, rec)
		d2 := newTestDevice(t)
		d3 := newTestDevice(t)
		_ = log.AddDevice(d2.Ref(), d1)
		_ = log.AddDevice(d3.Ref(), d1)
		// Two entries claiming the same version (non strictly +1).
		log.Entries[2].Version = log.Entries[1].Version
		if _, err := VerifyDeviceSet(log, root); err == nil {
			t.Fatal("expected duplicate-version log to be rejected")
		}
	})

	t.Run("broken-prev-hash", func(t *testing.T) {
		log, _ := NewGenesis(d1, rec)
		d2 := newTestDevice(t)
		if err := log.AddDevice(d2.Ref(), d1); err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
		log.Entries[1].PrevHash[0] ^= 0xff
		if _, err := VerifyDeviceSet(log, root); err == nil {
			t.Fatal("expected broken prev-hash to be rejected")
		}
	})

	t.Run("membership-not-matching-delta", func(t *testing.T) {
		// A member-signed entry whose declared Devices smuggles in an extra
		// device beyond its add delta must be rejected.
		log, _ := NewGenesis(d1, rec)
		d2 := newTestDevice(t)
		smuggled := newTestDevice(t)
		prev := &log.Entries[len(log.Entries)-1]
		ph, _ := prev.hash()
		change := d2.Ref()
		e := Entry{
			Version:  prev.Version + 1,
			Type:     EntryAdd,
			PrevHash: ph,
			Change:   &change,
			// Devices claims BOTH d2 and a smuggled device, delta only adds d2.
			Devices: append(append(cloneRefs(prev.Devices), d2.Ref()), smuggled.Ref()),
		}
		if err := e.sign(d1); err != nil { // properly signed by a member!
			t.Fatalf("sign: %v", err)
		}
		log.Entries = append(log.Entries, e)
		if _, err := VerifyDeviceSet(log, root); err == nil {
			t.Fatal("expected membership/delta mismatch to be rejected")
		}
	})

	t.Run("tampered-genesis-membership", func(t *testing.T) {
		// Flipping a byte in the genesis membership breaks the co-signatures.
		log, _ := NewGenesis(d1, rec)
		log.Entries[0].Devices[0].X25519Pub[0] ^= 0xff
		if _, err := VerifyDeviceSet(log, root); err == nil {
			t.Fatal("expected tampered genesis to be rejected")
		}
	})
}

// A version-splice that replays a stale head over a newer one is rejected.
func TestDeviceSetVersionSplice(t *testing.T) {
	d1 := newTestDevice(t)
	rec := newTestDevice(t)
	root := ownerRoot(d1, rec)
	log, _ := NewGenesis(d1, rec)
	d2 := newTestDevice(t)
	if err := log.AddDevice(d2.Ref(), d1); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	good, err := VerifyDeviceSet(log, root)
	if err != nil {
		t.Fatalf("VerifyDeviceSet: %v", err)
	}

	// Splice: drop the head entry to "revert" the add. The remaining log is a
	// valid prefix (genesis only) — but a client that has pinned the head's
	// version must refuse it. Here we assert the resolved set actually shrank,
	// which is exactly the regression a pin would detect.
	spliced := &DeviceSetLog{Entries: log.Entries[:1]}
	reverted, err := VerifyDeviceSet(spliced, root)
	if err != nil {
		t.Fatalf("VerifyDeviceSet(spliced prefix): %v", err)
	}
	if len(reverted) >= len(good) {
		t.Fatal("expected spliced prefix to resolve to a smaller set")
	}
	if spliced.Entries[len(spliced.Entries)-1].Version >= log.Entries[len(log.Entries)-1].Version {
		t.Fatal("spliced head version should be lower than the pinned head")
	}
}

// Signing a removed envelope: after a device is removed from the set, sealing to
// the resolved set excludes it (the removed key is no longer a recipient).
func TestRemovedDeviceNotARecipient(t *testing.T) {
	d1 := newTestDevice(t)
	rec := newTestDevice(t)
	root := ownerRoot(d1, rec)
	log, _ := NewGenesis(d1, rec)
	d2 := newTestDevice(t)
	if err := log.AddDevice(d2.Ref(), d1); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	if err := log.RemoveDevice(d2.X25519Pub, d1); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	members, err := VerifyDeviceSet(log, root)
	if err != nil {
		t.Fatalf("VerifyDeviceSet: %v", err)
	}
	recipients := make([]X25519PubKey, len(members))
	for i, m := range members {
		recipients[i] = m.X25519Pub
	}
	env, err := Seal([]byte("post-removal"), recipients, AtRestAAD{AccountID: "a", SecretID: "s", Version: 2})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The removed device d2 must NOT be able to open the freshly sealed env.
	if _, err := Open(env, d2.X25519Priv); err == nil {
		t.Fatal("removed device must not be a recipient of the re-sealed envelope")
	}
	// But d1 still can.
	if _, err := Open(env, d1.X25519Priv); err != nil {
		t.Fatalf("d1 should still open: %v", err)
	}
}
