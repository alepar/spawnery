package seal

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Note on Body encoding (WM9): Body is []byte, which JSON encodes as base64.
// Using []byte (not json.RawMessage) is intentional: json.RawMessage embeds bytes
// verbatim and json.MarshalIndent reformats them, changing the bytes and breaking
// signatures. []byte → base64 is stable across all JSON formatters.

// Device-set registry (§4, roast M4): an append-only, hash-chained, member-
// signed log of the owner's enrolled devices. The genesis entry is co-signed by
// device₁ + the recovery key; every mutation is signed by an EXISTING member's
// P-256 key. VerifyDeviceSet replays and validates the chain, rejecting
// unsigned, wrong-signer, or version-regressed logs — this is what stops a
// stolen-AS-session device injection: the AS stores the log but cannot author a
// valid member signature.
//
// WM9 canonical-bytes discipline: chain signatures and hashes are computed over
// the verbatim stored entry bytes — the raw Body field as authored and fetched,
// never re-serialized. This eliminates the cross-language canonical-JSON hazard.

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

// Signature is one member signature over an entry's Body bytes.
type Signature struct {
	// SignerPub is the SEC1-uncompressed P-256 public key of the signer.
	SignerPub []byte `json:"signer_pub"`
	// Sig is the ASN.1-DER ECDSA signature over sha256(Body).
	Sig []byte `json:"sig"`
}

// EntryLabel is the authenticated per-device label inside the signed Body (WM15).
// EnrolledAt is a decimal string of uint64 UnixNano — stored as a string (not a
// JSON number) to preserve full u64 precision without BigInt-in-JSON (WM10).
type EntryLabel struct {
	Name       string `json:"name"`
	EnrolledAt string `json:"enrolled_at"` // decimal u64 UnixNano string
}

// entryBody is the parsed view of a StoredEntry's Body bytes. It is decoded
// from Body for semantic use only; all signatures and hashes operate on the
// raw Body bytes (WM9: no re-serialization on either side).
type entryBody struct {
	Version  uint64      `json:"version"` // monotonic; genesis = 1
	Type     EntryType   `json:"type"`
	PrevHash []byte      `json:"prev"`            // SHA-256 of the previous StoredEntry; genesis = nil
	Change   *DeviceRef  `json:"change"`          // device added/removed (nil for genesis)
	Devices  []DeviceRef `json:"devices"`         // full membership AFTER this entry
	Label    *EntryLabel `json:"label,omitempty"` // WM15: authenticated label for the Change device
}

// StoredEntry is the on-the-wire / on-disk shape of one log entry (WM9).
// Body holds the exact authored JSON bytes of the entry; it is JSON-encoded as
// base64 ([]byte) so that the outer JSON formatter cannot reformat it and break
// signature verification. Sigs holds one or more member signatures, each over
// sha256(Body).
type StoredEntry struct {
	Body []byte      `json:"body"` // canonical JSON bytes of the entry body; base64 in JSON
	Sigs []Signature `json:"sigs"`
}

// parseBody decodes the Body bytes for semantic use only. Never re-marshal the
// result for hashing or signing — always operate on the raw Body bytes (WM9).
func (e *StoredEntry) parseBody() (entryBody, error) {
	var b entryBody
	if err := json.Unmarshal(e.Body, &b); err != nil {
		return entryBody{}, fmt.Errorf("seal: parse entry body: %w", err)
	}
	return b, nil
}

// hash is the chain link: SHA-256 over
//
//	encodeFields(Body, sig₀.SignerPub, sig₀.Sig, sig₁.SignerPub, sig₁.Sig, …)
//
// Deterministic length-prefixed binary (encodeFields from seal.go); a later
// entry's PrevHash commits to the prior Body and all signatures.
func (e *StoredEntry) hash() ([]byte, error) {
	if len(e.Sigs) == 0 {
		return nil, errors.New("seal: cannot hash an unsigned entry")
	}
	parts := make([][]byte, 0, 1+2*len(e.Sigs))
	parts = append(parts, []byte(e.Body))
	for _, s := range e.Sigs {
		parts = append(parts, s.SignerPub, s.Sig)
	}
	h := sha256.Sum256(encodeFields(parts...))
	return h[:], nil
}

// sign appends one ECDSA-P256 ASN.1-DER signature over sha256(Body) (WM9).
func (e *StoredEntry) sign(d *Device) error {
	digest := sha256.Sum256(e.Body)
	sig, err := ecdsa.SignASN1(rand.Reader, d.Sign, digest[:])
	if err != nil {
		return err
	}
	e.Sigs = append(e.Sigs, Signature{SignerPub: marshalSignPub(d.SignPub()), Sig: sig})
	return nil
}

// DeviceSetLog is the append-only chain.
type DeviceSetLog struct {
	Entries []StoredEntry `json:"entries"`
}

// HeadVersion returns the version number of the head entry (0 if the log is
// empty or the head body cannot be parsed).
func (l *DeviceSetLog) HeadVersion() uint64 {
	if len(l.Entries) == 0 {
		return 0
	}
	b, err := l.Entries[len(l.Entries)-1].parseBody()
	if err != nil {
		return 0
	}
	return b.Version
}

// HeadHash returns the chain hash of the head entry, or an error if the log is
// empty or unsigned.
func (l *DeviceSetLog) HeadHash() ([]byte, error) {
	if len(l.Entries) == 0 {
		return nil, errors.New("seal: empty log")
	}
	return l.Entries[len(l.Entries)-1].hash()
}

// HeadDevices returns the declared membership of the head entry for diagnostic
// display when the chain is invalid.  Prefer VerifyDeviceSet for trusted use.
func (l *DeviceSetLog) HeadDevices() []DeviceRef {
	if len(l.Entries) == 0 {
		return nil
	}
	b, err := l.Entries[len(l.Entries)-1].parseBody()
	if err != nil {
		return nil
	}
	return b.Devices
}

// OwnerRoot anchors trust: the two signing public keys (device₁ + recovery) that
// MUST co-sign genesis. Clients hold this root and verify the chain against it.
type OwnerRoot struct {
	Device1SignPub  []byte
	RecoverySignPub []byte
}

// Hash computes and returns the chain hash of this entry
// (sha256 of encodeFields(Body, sig₀.SignerPub, sig₀.Sig, …)).  This is the
// value stored as head_hash in the AS registry and committed as PrevHash by
// the next entry.  Exported so the AS can compute it without importing chain
// verification logic.
func (e *StoredEntry) Hash() ([]byte, error) { return e.hash() }

// VersionAndPrevHash decodes the Body and returns the entry's version number
// and prevHash.  The AS uses these for the CAS head-comparison without full
// chain verification (WM1: AS stores, never authors).
func (e *StoredEntry) VersionAndPrevHash() (version uint64, prevHash []byte, err error) {
	var b entryBody
	if err := json.Unmarshal(e.Body, &b); err != nil {
		return 0, nil, fmt.Errorf("seal: parse entry version/prev: %w", err)
	}
	return b.Version, b.PrevHash, nil
}

// buildEntry marshals an entryBody to JSON exactly once, producing the
// canonical Body bytes that will be stored and signed (WM9).
func buildEntry(b entryBody) (StoredEntry, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return StoredEntry{}, fmt.Errorf("seal: marshal entry body: %w", err)
	}
	return StoredEntry{Body: body}, nil
}

// nowNanoStr returns the current time as a decimal string of UnixNano (u64).
func nowNanoStr() string {
	return strconv.FormatUint(uint64(time.Now().UnixNano()), 10)
}

// NewGenesis creates a one-entry log whose genesis statement enrolls device1 and
// the recovery virtual device, co-signed by both of their signing keys (§4).
// Device1 gets an "device1" label; recovery's label name is "recovery" so the
// W3 UI can render it distinctly (WM15).
func NewGenesis(device1, recovery *Device) (*DeviceSetLog, error) {
	return NewGenesisLabeled(device1, recovery, "device1", "recovery")
}

// NewGenesisLabeled is like NewGenesis but accepts explicit device names (WM15).
func NewGenesisLabeled(device1, recovery *Device, device1Name, recoveryName string) (*DeviceSetLog, error) {
	if device1 == nil || recovery == nil {
		return nil, errors.New("seal: genesis needs device1 and recovery")
	}
	_ = recoveryName // documented: recovery label is implicit; the W3 label comes from device1Name
	b := entryBody{
		Version:  1,
		Type:     EntryGenesis,
		PrevHash: nil,
		Change:   nil,
		Devices:  []DeviceRef{device1.Ref(), recovery.Ref()},
		Label:    &EntryLabel{Name: device1Name, EnrolledAt: nowNanoStr()},
	}
	e, err := buildEntry(b)
	if err != nil {
		return nil, err
	}
	if err := e.sign(device1); err != nil {
		return nil, fmt.Errorf("seal: device1 sign genesis: %w", err)
	}
	if err := e.sign(recovery); err != nil {
		return nil, fmt.Errorf("seal: recovery sign genesis: %w", err)
	}
	return &DeviceSetLog{Entries: []StoredEntry{e}}, nil
}

// AddDevice appends a member-signed entry enrolling newDevice with an empty
// label name. signer must be a current member of the set.
func (l *DeviceSetLog) AddDevice(newDevice DeviceRef, signer *Device) error {
	return l.AddDeviceLabeled(newDevice, signer, "")
}

// AddDeviceLabeled is like AddDevice but records an authenticated label (WM15).
func (l *DeviceSetLog) AddDeviceLabeled(newDevice DeviceRef, signer *Device, name string) error {
	prev := l.head()
	if prev == nil {
		return errors.New("seal: empty log")
	}
	prevBody, err := prev.parseBody()
	if err != nil {
		return err
	}
	if memberIndex(prevBody.Devices, newDevice) >= 0 {
		return errors.New("seal: device already enrolled")
	}
	devices := append(cloneRefs(prevBody.Devices), newDevice)
	label := &EntryLabel{Name: name, EnrolledAt: nowNanoStr()}
	return l.appendMutation(EntryAdd, &newDevice, devices, signer, label)
}

// RemoveDevice appends a member-signed entry removing the device identified by
// its X25519 pubkey. signer must be a current member.
func (l *DeviceSetLog) RemoveDevice(targetX25519Pub []byte, signer *Device) error {
	prev := l.head()
	if prev == nil {
		return errors.New("seal: empty log")
	}
	prevBody, err := prev.parseBody()
	if err != nil {
		return err
	}
	idx := -1
	for i, d := range prevBody.Devices {
		if bytes.Equal(d.X25519Pub, targetX25519Pub) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("seal: device not enrolled")
	}
	removed := prevBody.Devices[idx]
	devices := append(cloneRefs(prevBody.Devices[:idx]), cloneRefs(prevBody.Devices[idx+1:])...)
	return l.appendMutation(EntryRemove, &removed, devices, signer, nil)
}

func (l *DeviceSetLog) appendMutation(t EntryType, change *DeviceRef, devices []DeviceRef, signer *Device, label *EntryLabel) error {
	prev := l.head()
	prevBody, err := prev.parseBody()
	if err != nil {
		return err
	}
	if memberIndex(prevBody.Devices, signer.Ref()) < 0 {
		return errors.New("seal: signer is not a current member")
	}
	ph, err := prev.hash()
	if err != nil {
		return err
	}
	b := entryBody{
		Version:  prevBody.Version + 1,
		Type:     t,
		PrevHash: ph,
		Change:   change,
		Devices:  devices,
		Label:    label,
	}
	e, err := buildEntry(b)
	if err != nil {
		return err
	}
	if err := e.sign(signer); err != nil {
		return err
	}
	l.Entries = append(l.Entries, e)
	return nil
}

func (l *DeviceSetLog) head() *StoredEntry {
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
//
// WM9: signatures and hashes are verified over the raw Body bytes (no
// re-serialization on either side of the chain).
func VerifyDeviceSet(l *DeviceSetLog, root OwnerRoot) ([]DeviceRef, error) {
	if l == nil || len(l.Entries) == 0 {
		return nil, errors.New("seal: empty device-set log")
	}

	// Genesis.
	g := &l.Entries[0]
	gb, err := g.parseBody()
	if err != nil {
		return nil, err
	}
	if gb.Type != EntryGenesis {
		return nil, errors.New("seal: first entry is not genesis")
	}
	if gb.Version != 1 {
		return nil, errors.New("seal: genesis version must be 1")
	}
	if len(gb.PrevHash) != 0 {
		return nil, errors.New("seal: genesis must have no prev-hash")
	}
	if err := verifyGenesisSigs(g, root); err != nil {
		return nil, err
	}

	prev := g
	prevBody := gb
	members := cloneRefs(gb.Devices)

	for i := 1; i < len(l.Entries); i++ {
		e := &l.Entries[i]
		eb, err := e.parseBody()
		if err != nil {
			return nil, err
		}
		if eb.Version != prevBody.Version+1 {
			return nil, fmt.Errorf("seal: entry %d version %d not monotonic (expected %d)", i, eb.Version, prevBody.Version+1)
		}
		ph, err := prev.hash()
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(eb.PrevHash, ph) {
			return nil, fmt.Errorf("seal: entry %d prev-hash mismatch (chain broken)", i)
		}
		// Recompute expected membership from the declared delta over the prior
		// members, and require the entry's Devices to match exactly.
		expected, err := applyDelta(members, eb)
		if err != nil {
			return nil, fmt.Errorf("seal: entry %d: %w", i, err)
		}
		if !sameSet(expected, eb.Devices) {
			return nil, fmt.Errorf("seal: entry %d declared membership does not match its delta", i)
		}
		// Must be signed by a member of the PRIOR set (WM9: over raw Body bytes).
		if err := verifyMemberSig(e, members); err != nil {
			return nil, fmt.Errorf("seal: entry %d: %w", i, err)
		}
		members = expected
		prev = e
		prevBody = eb
	}
	return members, nil
}

func verifyGenesisSigs(g *StoredEntry, root OwnerRoot) error {
	// WM9: sign/verify over raw Body bytes.
	digest := sha256.Sum256(g.Body)
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

func verifyMemberSig(e *StoredEntry, members []DeviceRef) error {
	// WM9: sign/verify over raw Body bytes.
	digest := sha256.Sum256(e.Body)
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
func applyDelta(prev []DeviceRef, e entryBody) ([]DeviceRef, error) {
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
