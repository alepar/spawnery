// Package journal implements Spawnery's transient storage tier: per-spawn,
// client-side-encrypted Kopia journaling of mount host dirs, for lossless
// same-node suspend/resume and crash recovery.
//
// Design: docs/superpowers/specs/2026-06-10-transient-tier-kopia-journal-design.md
//
// This package is the node-local phase ① slice (sp-u53.5.1): it embeds Kopia
// (github.com/kopia/kopia) as a library over a pluggable blob backend
// (filesystem for hermetic tests; an S3/Garage backend slots in later — see
// blob.go), with node-held key custody (custody.go), a per-mount serialized
// snapshot queue + suspend barrier (queue.go), and an adaptive debounce
// scheduler (debounce.go).
//
// Out of scope for this slice (left as TODO(phase②) seams): CP-protocol
// generation/marker threading, per-generation Garage key mint/revoke, the
// plaintext durability sentinel, CP-commanded full maintenance, and the
// owner-sealed password delivery sub-protocol.
package journal

import (
	"context"
	"fmt"
)

// DurabilityClass is a mount's per-mount durability promise (design §1a). It
// drives whether the journaler captures the mount and how its repo password is
// custodied.
type DurabilityClass int

const (
	// Ephemeral mounts are never journaled — suspend resets them (today's
	// scratch contract). The journaler skips them entirely.
	Ephemeral DurabilityClass = iota
	// NodeLocal mounts are journaled with a node-held repo password
	// (custody.go). They survive same-node suspend/resume + crash, but a
	// different node cannot resume them (it lacks the password).
	NodeLocal
	// OwnerSealed mounts are journaled identically to NodeLocal for THIS slice;
	// they differ only in that the password is custodied externally (sealed to
	// the owner) via a PasswordProvider. Owner-sealed delivery itself is not
	// implemented here — only the seam (see PasswordProvider).
	OwnerSealed
)

func (d DurabilityClass) String() string {
	switch d {
	case Ephemeral:
		return "ephemeral"
	case NodeLocal:
		return "node-local"
	case OwnerSealed:
		return "owner-sealed"
	default:
		return fmt.Sprintf("DurabilityClass(%d)", int(d))
	}
}

// Journaled reports whether a mount of this class is captured by the journaler.
func (d DurabilityClass) Journaled() bool { return d == NodeLocal || d == OwnerSealed }

// ParseDurability maps a manifest/spawn.yml `durability:` string to a class.
// The empty string and "ephemeral" both map to Ephemeral so the default
// (unset) preserves today's scratch contract — the journaler is a no-op until a
// mount opts in.
func ParseDurability(s string) (DurabilityClass, error) {
	switch s {
	case "", "ephemeral":
		return Ephemeral, nil
	case "node-local":
		return NodeLocal, nil
	case "owner-sealed":
		return OwnerSealed, nil
	default:
		return Ephemeral, fmt.Errorf("unknown durability class %q", s)
	}
}

// ManifestID is a pinned Kopia snapshot manifest id (design §3, roast C1).
// Restores take an explicit ManifestID, never "latest".
type ManifestID string

func (m ManifestID) String() string { return string(m) }

// Mount is the journaler's view of one mount: its name, the host dir to
// snapshot/restore, its durability class, and whether it is a secret tmpfs
// mount (excluded from the journal by mount path — no content scan, design §2).
type Mount struct {
	Name    string
	HostDir string
	Class   DurabilityClass
	Secret  bool
}

// shouldJournal reports whether this mount is captured: a journaled class and
// not a secret mount.
func (m Mount) shouldJournal() bool { return m.Class.Journaled() && !m.Secret }

// PasswordProvider custodies the per-spawn Kopia repo password (design §4). The
// CP is never a custodian in any class. NodeLocalCustody (custody.go) is the
// node-local implementation; an owner-sealed provider plugs in later behind
// this same seam (do not implement owner-sealed delivery in this slice).
type PasswordProvider interface {
	// PasswordFor returns the repo password for spawnID, generating and sealing
	// a fresh one on first call and returning the same password on subsequent
	// same-node calls.
	PasswordFor(spawnID string) (string, error)
	// Forget drops the sealed password for spawnID (spawn delete / migrate-away).
	Forget(spawnID string) error
}

// JournalManager is the seam the spawnlet wires to (manager.go). All methods
// are no-ops for spawns/mounts that are not journaled, so scratch-only spawns
// are unaffected.
type JournalManager interface {
	// RequestSnapshot schedules an async, debounced, serialized snapshot of one
	// journaled mount. A no-op for ephemeral/secret mounts.
	RequestSnapshot(ctx context.Context, spawnID string, gen uint64, m Mount)

	// FinalSnapshot drains pending work for each journaled mount of the spawn
	// (the suspend barrier, design §2 roast M17), takes the final snapshot, and
	// returns the per-mount pinned manifest ids (keyed by mount name). Mounts
	// that are not journaled are absent from the result.
	FinalSnapshot(ctx context.Context, spawnID string, gen uint64, mounts []Mount) (map[string]ManifestID, error)

	// Restore restores a pinned manifest into hostDir before bind (design §3,
	// roast C1 — explicit manifest id, never "latest").
	Restore(ctx context.Context, spawnID, mountName string, id ManifestID, hostDir string) error

	// LatestForGeneration returns the latest COMPLETE manifest for (mount,
	// generation) — the crash fallback ONLY (design §2/§3). The primary restore
	// path is Restore by pinned id.
	LatestForGeneration(ctx context.Context, spawnID, mountName string, gen uint64) (ManifestID, error)

	// QuickMaintenance runs index-compacting (non-deleting) maintenance on the
	// spawn's repo (design §2 roast M5). Cadence-driving is out of scope.
	QuickMaintenance(ctx context.Context, spawnID string) error

	// Close releases the spawn's repo handle and in-memory scheduler state.
	Close(ctx context.Context, spawnID string) error
}
