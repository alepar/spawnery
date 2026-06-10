package seal

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// Device-set registry (§4, roast M4): an append-only, hash-chained, member-
// signed log of the owner's enrolled devices. The genesis entry is co-signed by
// device₁ + the recovery key; every mutation is signed by an EXISTING member's
// P-256 key. VerifyDeviceSet replays and validates the chain, rejecting
// unsigned, wrong-signer, or version-regressed logs — this is what stops a
// stolen-AS-session device injection: the AS stores the log but cannot author a
// valid member signature.

// EntryType discriminates the log entries.
type EntryType string

const (
	EntryGenesis EntryType = "genesis"
	EntryAdd     EntryType = "add"
	EntryRemove  EntryType = "remove"
)

// DeviceRef is the public identity of an enrolled device: its X25519 (HPKE) and
// P-256 (signing, SEC1-uncompressed) public keys.
type DeviceRef struct {
	X25519Pub []byte `json:"x25519_pub"`
	SignPub   []byte `json:"sign_pub"`
}

func (r DeviceRef) equal(o DeviceRef) bool {
	return bytes.Equal(r.X25519Pub, o.X25519Pub) && bytes.Equal(r.SignPub, o.SignPub)
}

// Signature is one member signature over an entry's signed body.
type Signature struct {
	// SignerPub is the SEC1-uncompressed P-256 public key of the signer.
	SignerPub []byte `json:"signer_pub"`
	// Sig is the ASN.1-DER ECDSA signature.
	Sig []byte `json:"sig"`
}

// Entry is one append-only record. Devices holds the FULL resolved membership
// after applying this entry, so an entry cannot misrepresent the set without
// breaking the hash chain or a signature.
type Entry struct {
	Version  uint64      `json:"version"` // monotonic; genesis = 1
	Type     EntryType   `json:"type"`    //
	PrevHash []byte      `json:"prev"`    // SHA-256 of the previous entry; genesis = nil
	Change   *DeviceRef  `json:"change"`  // the device added/removed (nil for genesis)
	Devices  []DeviceRef `json:"devices"` // full membership AFTER this entry
	Sigs     []Signature `json:"sigs"`    //
}

// signedBody is the canonical byte string each member signs (everything but the
// signatures). encoding/json over a fixed-field struct is deterministic.
func (e *Entry) signedBody() ([]byte, error) {
	tmp := struct {
		Version  uint64      `json:"version"`
		Type     EntryType   `json:"type"`
		PrevHash []byte      `json:"prev"`
		Change   *DeviceRef  `json:"change"`
		Devices  []DeviceRef `json:"devices"`
	}{e.Version, e.Type, e.PrevHash, e.Change, e.Devices}
	b, err := json.Marshal(tmp)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// hash is the chain link: SHA-256 over the full entry (body + signatures), so a
// later entry's PrevHash commits to the prior entry's signatures too.
func (e *Entry) hash() ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	return h[:], nil
}

func (e *Entry) sign(d *Device) error {
	body, err := e.signedBody()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	sig, err := ecdsa.SignASN1(rand.Reader, d.Sign, digest[:])
	if err != nil {
		return err
	}
	e.Sigs = append(e.Sigs, Signature{SignerPub: marshalSignPub(d.SignPub()), Sig: sig})
	return nil
}

// DeviceSetLog is the append-only chain.
type DeviceSetLog struct {
	Entries []Entry `json:"entries"`
}

// OwnerRoot anchors trust: the two signing public keys (device₁ + recovery) that
// MUST co-sign genesis. Clients hold this root and verify the chain against it.
type OwnerRoot struct {
	Device1SignPub  []byte
	RecoverySignPub []byte
}

// NewGenesis creates a one-entry log whose genesis statement enrolls device1 and
// the recovery virtual device, co-signed by both of their signing keys (§4).
func NewGenesis(device1, recovery *Device) (*DeviceSetLog, error) {
	if device1 == nil || recovery == nil {
		return nil, errors.New("seal: genesis needs device1 and recovery")
	}
	e := Entry{
		Version:  1,
		Type:     EntryGenesis,
		PrevHash: nil,
		Change:   nil,
		Devices:  []DeviceRef{device1.Ref(), recovery.Ref()},
	}
	if err := e.sign(device1); err != nil {
		return nil, fmt.Errorf("seal: device1 sign genesis: %w", err)
	}
	if err := e.sign(recovery); err != nil {
		return nil, fmt.Errorf("seal: recovery sign genesis: %w", err)
	}
	return &DeviceSetLog{Entries: []Entry{e}}, nil
}

// AddDevice appends a member-signed entry enrolling newDevice. signer must be a
// current member of the set.
func (l *DeviceSetLog) AddDevice(newDevice DeviceRef, signer *Device) error {
	prev := l.head()
	if prev == nil {
		return errors.New("seal: empty log")
	}
	if memberIndex(prev.Devices, newDevice) >= 0 {
		return errors.New("seal: device already enrolled")
	}
	devices := append(cloneRefs(prev.Devices), newDevice)
	return l.appendMutation(EntryAdd, &newDevice, devices, signer)
}

// RemoveDevice appends a member-signed entry removing the device identified by
// its X25519 pubkey. signer must be a current member.
func (l *DeviceSetLog) RemoveDevice(targetX25519Pub []byte, signer *Device) error {
	prev := l.head()
	if prev == nil {
		return errors.New("seal: empty log")
	}
	idx := -1
	for i, d := range prev.Devices {
		if bytes.Equal(d.X25519Pub, targetX25519Pub) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("seal: device not enrolled")
	}
	removed := prev.Devices[idx]
	devices := append(cloneRefs(prev.Devices[:idx]), cloneRefs(prev.Devices[idx+1:])...)
	return l.appendMutation(EntryRemove, &removed, devices, signer)
}

func (l *DeviceSetLog) appendMutation(t EntryType, change *DeviceRef, devices []DeviceRef, signer *Device) error {
	prev := l.head()
	if memberIndex(prev.Devices, signer.Ref()) < 0 {
		return errors.New("seal: signer is not a current member")
	}
	ph, err := prev.hash()
	if err != nil {
		return err
	}
	e := Entry{
		Version:  prev.Version + 1,
		Type:     t,
		PrevHash: ph,
		Change:   change,
		Devices:  devices,
	}
	if err := e.sign(signer); err != nil {
		return err
	}
	l.Entries = append(l.Entries, e)
	return nil
}

func (l *DeviceSetLog) head() *Entry {
	if len(l.Entries) == 0 {
		return nil
	}
	return &l.Entries[len(l.Entries)-1]
}

// VerifyDeviceSet replays and validates the full chain against ownerRoot,
// returning the resolved membership at the head. It REJECTS: a genesis not
// co-signed by both owner roots, an entry not strictly version+1 (regress or
// dup), a broken prev-hash link, an entry whose declared membership does not
// match the add/remove delta, and any mutation not signed by a current member
// (the stolen-AS-session injection defense).
func VerifyDeviceSet(l *DeviceSetLog, root OwnerRoot) ([]DeviceRef, error) {
	if l == nil || len(l.Entries) == 0 {
		return nil, errors.New("seal: empty device-set log")
	}

	// Genesis.
	g := &l.Entries[0]
	if g.Type != EntryGenesis {
		return nil, errors.New("seal: first entry is not genesis")
	}
	if g.Version != 1 {
		return nil, errors.New("seal: genesis version must be 1")
	}
	if len(g.PrevHash) != 0 {
		return nil, errors.New("seal: genesis must have no prev-hash")
	}
	if err := verifyGenesisSigs(g, root); err != nil {
		return nil, err
	}

	prev := g
	members := cloneRefs(g.Devices)

	for i := 1; i < len(l.Entries); i++ {
		e := &l.Entries[i]
		if e.Version != prev.Version+1 {
			return nil, fmt.Errorf("seal: entry %d version %d not monotonic (expected %d)", i, e.Version, prev.Version+1)
		}
		ph, err := prev.hash()
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(e.PrevHash, ph) {
			return nil, fmt.Errorf("seal: entry %d prev-hash mismatch (chain broken)", i)
		}
		// Recompute expected membership from the declared delta over the prior
		// members, and require the entry's Devices to match exactly.
		expected, err := applyDelta(members, e)
		if err != nil {
			return nil, fmt.Errorf("seal: entry %d: %w", i, err)
		}
		if !sameSet(expected, e.Devices) {
			return nil, fmt.Errorf("seal: entry %d declared membership does not match its delta", i)
		}
		// Must be signed by a member of the PRIOR set.
		if err := verifyMemberSig(e, members); err != nil {
			return nil, fmt.Errorf("seal: entry %d: %w", i, err)
		}
		members = expected
		prev = e
	}
	return members, nil
}

func verifyGenesisSigs(g *Entry, root OwnerRoot) error {
	body, err := g.signedBody()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	haveDev1, haveRec := false, false
	for _, s := range g.Sigs {
		if bytes.Equal(s.SignerPub, root.Device1SignPub) && verifySig(root.Device1SignPub, digest[:], s.Sig) {
			haveDev1 = true
		}
		if bytes.Equal(s.SignerPub, root.RecoverySignPub) && verifySig(root.RecoverySignPub, digest[:], s.Sig) {
			haveRec = true
		}
	}
	if !haveDev1 || !haveRec {
		return errors.New("seal: genesis not co-signed by device1 + recovery owner roots")
	}
	return nil
}

func verifyMemberSig(e *Entry, members []DeviceRef) error {
	body, err := e.signedBody()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	for _, s := range e.Sigs {
		// signer must be a current member AND the signature must verify.
		if !memberHasSignPub(members, s.SignerPub) {
			continue
		}
		if verifySig(s.SignerPub, digest[:], s.Sig) {
			return nil
		}
	}
	return errors.New("not signed by a current member")
}

func verifySig(signerPub, digest, sig []byte) bool {
	pub, err := parseSignPub(signerPub)
	if err != nil {
		return false
	}
	return ecdsa.VerifyASN1(pub, digest, sig)
}

// applyDelta computes the expected membership after an entry, from its type and
// Change field, validating the delta is well-formed.
func applyDelta(prev []DeviceRef, e *Entry) ([]DeviceRef, error) {
	switch e.Type {
	case EntryAdd:
		if e.Change == nil {
			return nil, errors.New("add entry missing change")
		}
		if memberIndex(prev, *e.Change) >= 0 {
			return nil, errors.New("add of already-enrolled device")
		}
		return append(cloneRefs(prev), *e.Change), nil
	case EntryRemove:
		if e.Change == nil {
			return nil, errors.New("remove entry missing change")
		}
		idx := memberIndex(prev, *e.Change)
		if idx < 0 {
			return nil, errors.New("remove of non-member device")
		}
		return append(cloneRefs(prev[:idx]), cloneRefs(prev[idx+1:])...), nil
	case EntryGenesis:
		return nil, errors.New("unexpected genesis after head")
	default:
		return nil, fmt.Errorf("unknown entry type %q", e.Type)
	}
}

func memberIndex(set []DeviceRef, d DeviceRef) int {
	for i, m := range set {
		if m.equal(d) {
			return i
		}
	}
	return -1
}

func memberHasSignPub(set []DeviceRef, signPub []byte) bool {
	for _, m := range set {
		if bytes.Equal(m.SignPub, signPub) {
			return true
		}
	}
	return false
}

func sameSet(a, b []DeviceRef) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if memberIndex(b, x) < 0 {
			return false
		}
	}
	return true
}

func cloneRefs(in []DeviceRef) []DeviceRef {
	out := make([]DeviceRef, len(in))
	for i, r := range in {
		out[i] = DeviceRef{
			X25519Pub: append([]byte(nil), r.X25519Pub...),
			SignPub:   append([]byte(nil), r.SignPub...),
		}
	}
	return out
}
