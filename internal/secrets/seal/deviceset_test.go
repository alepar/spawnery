package seal

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func ownerRoot(device1, recovery *Device) OwnerRoot {
	return OwnerRoot{
		Device1SignPub:  marshalSignPub(device1.SignPub()),
		RecoverySignPub: marshalSignPub(recovery.SignPub()),
	}
}

// tamperBody returns a new StoredEntry with modified body bytes (but the
// original signatures kept) by applying fn to the parsed entryBody. The
// resulting signature is intentionally broken — tests use this to assert that
// the verifier rejects tampered entries for any reason.
func tamperBody(e StoredEntry, fn func(b *entryBody)) StoredEntry {
	var b entryBody
	if err := json.Unmarshal(e.Body, &b); err != nil {
		panic("tamperBody: unmarshal: " + err.Error())
	}
	fn(&b)
	newBody, err := json.Marshal(b)
	if err != nil {
		panic("tamperBody: marshal: " + err.Error())
	}
	return StoredEntry{Body: newBody, Sigs: e.Sigs}
}

// mustParseBody is a test helper.
func (e StoredEntry) mustParseBody(t *testing.T) entryBody {
	t.Helper()
	b, err := e.parseBody()
	if err != nil {
		t.Fatalf("parseBody: %v", err)
	}
	return b
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
	badBody := entryBody{Version: 1, Type: EntryGenesis, Devices: []DeviceRef{d1.Ref(), rec.Ref()}}
	bad, err := buildEntry(badBody)
	if err != nil {
		t.Fatalf("buildEntry: %v", err)
	}
	if err := bad.sign(d1); err != nil {
		t.Fatalf("sign: %v", err)
	}
	log := &DeviceSetLog{Entries: []StoredEntry{bad}}
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
		prev := log.head()
		prevBody, _ := prev.parseBody()
		ph, _ := prev.hash()
		change := injected.Ref()
		b := entryBody{
			Version:  prevBody.Version + 1,
			Type:     EntryAdd,
			PrevHash: ph,
			Change:   &change,
			Devices:  append(cloneRefs(prevBody.Devices), injected.Ref()),
		}
		e, err := buildEntry(b)
		if err != nil {
			t.Fatalf("buildEntry: %v", err)
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
		prev := log.head()
		prevBody, _ := prev.parseBody()
		ph, _ := prev.hash()
		change := injected.Ref()
		b := entryBody{
			Version:  prevBody.Version + 1,
			Type:     EntryAdd,
			PrevHash: ph,
			Change:   &change,
			Devices:  append(cloneRefs(prevBody.Devices), injected.Ref()),
			// no Sigs
		}
		e, _ := buildEntry(b)
		// Append without signing — Sigs is empty.
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
		// tamperBody keeps old sigs (so sig is invalid too, but version check comes first).
		log.Entries[1] = tamperBody(log.Entries[1], func(b *entryBody) { b.Version = 1 })
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
		v1 := log.Entries[1].mustParseBody(t).Version
		log.Entries[2] = tamperBody(log.Entries[2], func(b *entryBody) { b.Version = v1 })
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
		// Flip a byte of PrevHash inside the Body.
		log.Entries[1] = tamperBody(log.Entries[1], func(b *entryBody) { b.PrevHash[0] ^= 0xff })
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
		prev := log.head()
		prevBody, _ := prev.parseBody()
		ph, _ := prev.hash()
		change := d2.Ref()
		b := entryBody{
			Version:  prevBody.Version + 1,
			Type:     EntryAdd,
			PrevHash: ph,
			Change:   &change,
			// Devices claims BOTH d2 and a smuggled device, delta only adds d2.
			Devices: append(append(cloneRefs(prevBody.Devices), d2.Ref()), smuggled.Ref()),
		}
		e, err := buildEntry(b)
		if err != nil {
			t.Fatalf("buildEntry: %v", err)
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
		// Flipping a byte in the genesis Body breaks the co-signatures.
		log, _ := NewGenesis(d1, rec)
		// Tamper: flip a byte in the first device's X25519Pub inside the Body.
		log.Entries[0] = tamperBody(log.Entries[0], func(b *entryBody) {
			b.Devices[0].X25519Pub[0] ^= 0xff
		})
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
	if spliced.HeadVersion() >= log.HeadVersion() {
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

// TestEntryLabelRoundTrip verifies that an EntryLabel is included in the Body
// and survives a marshal→unmarshal round-trip (WM15).
func TestEntryLabelRoundTrip(t *testing.T) {
	d1 := newTestDevice(t)
	rec := newTestDevice(t)
	root := ownerRoot(d1, rec)

	log, err := NewGenesisLabeled(d1, rec, "my-laptop", "recovery")
	if err != nil {
		t.Fatalf("NewGenesisLabeled: %v", err)
	}

	// Both device1 and recovery labels are inside the signed Body (WM15).
	genesisBody, err := log.Entries[0].parseBody()
	if err != nil {
		t.Fatalf("parseBody: %v", err)
	}
	if genesisBody.Label == nil || genesisBody.Label.Name != "my-laptop" {
		t.Fatalf("expected genesis label name %q, got %+v", "my-laptop", genesisBody.Label)
	}
	if genesisBody.Label.EnrolledAt == "" {
		t.Fatal("genesis label enrolled_at is empty")
	}
	if genesisBody.RecoveryLabel == nil || genesisBody.RecoveryLabel.Name != "recovery" {
		t.Fatalf("expected genesis recovery_label name %q, got %+v", "recovery", genesisBody.RecoveryLabel)
	}
	if genesisBody.RecoveryLabel.EnrolledAt == "" {
		t.Fatal("genesis recovery_label enrolled_at is empty")
	}

	// Add a labeled device.
	d2 := newTestDevice(t)
	if err := log.AddDeviceLabeled(d2.Ref(), d1, "work-phone"); err != nil {
		t.Fatalf("AddDeviceLabeled: %v", err)
	}
	addBody, err := log.Entries[1].parseBody()
	if err != nil {
		t.Fatalf("parseBody add: %v", err)
	}
	if addBody.Label == nil || addBody.Label.Name != "work-phone" {
		t.Fatalf("expected add label %q, got %+v", "work-phone", addBody.Label)
	}

	// Full chain still verifies.
	if _, err := VerifyDeviceSet(log, root); err != nil {
		t.Fatalf("VerifyDeviceSet with labels: %v", err)
	}
}

// TestEntryBodyTamperedFails ensures that modifying the Body bytes (even while
// keeping the original signatures) causes verification to fail (WM9).
func TestEntryBodyTamperedFails(t *testing.T) {
	d1 := newTestDevice(t)
	rec := newTestDevice(t)
	root := ownerRoot(d1, rec)

	log, _ := NewGenesis(d1, rec)
	d2 := newTestDevice(t)
	_ = log.AddDevice(d2.Ref(), d1)

	// Tamper with the add entry's Body (keep original sigs).
	origBody := log.Entries[1].Body
	tampered := make([]byte, len(origBody))
	copy(tampered, origBody)
	// Flip a byte to corrupt the body.
	tampered[len(tampered)/2] ^= 0x01
	log.Entries[1] = StoredEntry{Body: tampered, Sigs: log.Entries[1].Sigs}

	if _, err := VerifyDeviceSet(log, root); err == nil {
		t.Fatal("expected tampered body to be rejected")
	}
}

// TestEntrySigTamperedFails ensures that modifying a signature (while keeping
// the original Body) causes verification to fail (WM9).
func TestEntrySigTamperedFails(t *testing.T) {
	d1 := newTestDevice(t)
	rec := newTestDevice(t)
	root := ownerRoot(d1, rec)

	log, _ := NewGenesis(d1, rec)
	d2 := newTestDevice(t)
	_ = log.AddDevice(d2.Ref(), d1)

	// Flip a byte in the first signature of the add entry.
	addEntry := log.Entries[1]
	tamperedSigs := make([]Signature, len(addEntry.Sigs))
	copy(tamperedSigs, addEntry.Sigs)
	tamperedSigs[0].Sig = append([]byte(nil), tamperedSigs[0].Sig...)
	tamperedSigs[0].Sig[0] ^= 0xff
	log.Entries[1] = StoredEntry{Body: addEntry.Body, Sigs: tamperedSigs}

	if _, err := VerifyDeviceSet(log, root); err == nil {
		t.Fatal("expected tampered sig to be rejected")
	}
}

// TestEnrolledAtIsDecimalNano verifies that EnrolledAt is a decimal string of
// UnixNano (non-zero; parseable as uint64; no fractional part) (WM10).
func TestEnrolledAtIsDecimalNano(t *testing.T) {
	d1 := newTestDevice(t)
	rec := newTestDevice(t)
	log, err := NewGenesis(d1, rec)
	if err != nil {
		t.Fatalf("NewGenesis: %v", err)
	}
	b, err := log.Entries[0].parseBody()
	if err != nil {
		t.Fatalf("parseBody: %v", err)
	}
	if b.Label == nil {
		t.Fatal("genesis has no label")
	}
	nano := b.Label.EnrolledAt
	if nano == "" {
		t.Fatal("enrolled_at is empty")
	}
	// Must be all decimal digits.
	for _, ch := range nano {
		if ch < '0' || ch > '9' {
			t.Fatalf("enrolled_at %q contains non-digit %q", nano, ch)
		}
	}
	// Must not look like a float.
	if strings.ContainsAny(nano, ".e+E") {
		t.Fatalf("enrolled_at %q looks like a float", nano)
	}
	// Must not be zero.
	if nano == "0" {
		t.Fatal("enrolled_at is zero")
	}
}

// TestRawBytesChainVerifies asserts that the chain built via buildEntry
// (never re-marshalled) verifies cleanly, and that HeadVersion / HeadHash
// are consistent with the chain state.
func TestRawBytesChainVerifies(t *testing.T) {
	d1 := newTestDevice(t)
	rec := newTestDevice(t)
	root := ownerRoot(d1, rec)

	log, _ := NewGenesis(d1, rec)
	d2 := newTestDevice(t)
	_ = log.AddDevice(d2.Ref(), d1)
	_ = log.RemoveDevice(d2.X25519Pub, d1)

	// The chain built via buildEntry (never re-marshalled) must verify cleanly.
	if _, err := VerifyDeviceSet(log, root); err != nil {
		t.Fatalf("raw-bytes chain rejected: %v", err)
	}

	// Also ensure HeadVersion and HeadHash are consistent.
	if v := log.HeadVersion(); v != 3 {
		t.Fatalf("HeadVersion = %d, want 3", v)
	}
	h, err := log.HeadHash()
	if err != nil {
		t.Fatalf("HeadHash: %v", err)
	}
	if len(h) != 32 {
		t.Fatalf("HeadHash length = %d, want 32", len(h))
	}
	// A different log has a different head hash.
	log2, _ := NewGenesis(d1, rec)
	h2, _ := log2.HeadHash()
	if bytes.Equal(h, h2) {
		t.Fatal("different logs produced the same head hash")
	}
}
