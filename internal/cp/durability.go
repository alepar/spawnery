package cp

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"spawnery/internal/cp/journalkeys"
	"spawnery/internal/cp/store"
	"spawnery/internal/manifest"
	"spawnery/internal/secrets/journalkey"
)

// mountClass is the resolved durability class for a single mount.
type mountClass int

const (
	mountClassEphemeral   mountClass = iota // no journaling
	mountClassNodeLocal                     // journaled, key node-held
	mountClassOwnerSealed                   // journaled, key owner-sealed; cross-node restorable
)

// classifyMounts returns the durability class for each mount of spawnID.
// It resolves in priority order:
//  1. If a journal-key ciphertext exists in the CP store → owner-sealed.
//  2. Else read the app-version manifest's declared durability (via the stored protojson blob).
//  3. Absent/empty durability → ephemeral.
//
// This is used by the MigrateSpawn durability guard: a cross-node move of a node-local
// mount fails unless upgrade_to_owner_sealed is set AND the ciphertext now exists.
func (s *Server) classifyMounts(ctx context.Context, spawnID string) (map[string]mountClass, error) {
	mounts, err := s.st.Spawns().GetMounts(ctx, spawnID)
	if err != nil {
		return nil, fmt.Errorf("classifyMounts: GetMounts: %w", err)
	}
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return nil, fmt.Errorf("classifyMounts: Get spawn: %w", err)
	}

	// Look up manifest durability declarations. A version may have no manifest or an older
	// registration without durability; those default to ephemeral.
	var manifestDurability map[string]string
	if ver, verErr := s.st.Apps().GetVersion(ctx, sp.AppID, sp.AppVersion); verErr == nil {
		if md, mdErr := manifest.MountDurabilityFromJSON(ver.Manifest); mdErr == nil {
			manifestDurability = md
		}
	}

	result := make(map[string]mountClass, len(mounts))
	for _, m := range mounts {
		// Priority 1: owner-sealed ciphertext exists → owner-sealed.
		_, gerr := s.journalKeys.Get(ctx, spawnID, journalkey.SecretID(m.Name))
		if gerr == nil {
			result[m.Name] = mountClassOwnerSealed
			continue
		}
		if !errors.Is(gerr, journalkeys.ErrNotFound) {
			return nil, fmt.Errorf("classifyMounts: journalKeys.Get(%q): %w", m.Name, gerr)
		}

		// Priority 2: declared durability in the manifest.
		decl := manifestDurability[m.Name]
		switch decl {
		case "node-local":
			result[m.Name] = mountClassNodeLocal
		case "owner-sealed":
			// Declared owner-sealed but no ciphertext yet → treat as node-local until upgraded.
			// This covers newly-registered app versions before the lazy ceremony.
			result[m.Name] = mountClassNodeLocal
		default:
			result[m.Name] = mountClassEphemeral
		}
	}
	return result, nil
}

// guardCrossNodeDurability is the MigrateSpawn durability-class guard. It is called after the
// tenancy pre-check, BEFORE suspendLocked. For a cross-node move it:
//   - Rejects node-local mounts unless upgradeToOwnerSealed is set AND the ciphertext now exists
//     (fails closed: the upgrade must have already completed before calling MigrateSpawn).
//   - Allows ephemeral mounts (data does not travel — the caller shows a warning in the UI).
//   - Allows owner-sealed mounts (they are cross-node restorable by design).
//   - Same-node moves skip the guard entirely.
//
// Returns connect.CodeFailedPrecondition on violation (spawn left untouched).
func (s *Server) guardCrossNodeDurability(ctx context.Context, spawnID, liveNodeID, targetNodeID string, upgradeToOwnerSealed bool) error {
	// Determine if this is a cross-node move. A class-target is conservative: treat as cross-node.
	if targetNodeID != "" && targetNodeID == liveNodeID {
		return nil // same-node: no guard needed
	}

	classes, err := s.classifyMounts(ctx, spawnID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}

	mounts, err := s.st.Spawns().GetMounts(ctx, spawnID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}

	for _, m := range mounts {
		mc := classes[m.Name]
		if mc != mountClassNodeLocal {
			continue // ephemeral or owner-sealed: OK
		}
		// node-local mount on a cross-node move.
		if !upgradeToOwnerSealed {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("moving requires upgrading this spawn's storage to owner-sealed (mount %q is node-local); set upgrade_to_owner_sealed after completing UpgradeToOwnerSealed", m.Name))
		}
		// upgrade flag set: re-verify the ciphertext now exists (fail-closed if the upgrade didn't land).
		if _, gerr := s.journalKeys.Get(ctx, spawnID, journalkey.SecretID(m.Name)); gerr != nil {
			if errors.Is(gerr, journalkeys.ErrNotFound) {
				return connect.NewError(connect.CodeFailedPrecondition,
					fmt.Errorf("upgrade_to_owner_sealed set but mount %q still has no owner-sealed ciphertext; ensure UpgradeToOwnerSealed completed", m.Name))
			}
			return connect.NewError(connect.CodeInternal, gerr)
		}
	}
	return nil
}

// liveNodeForSpawn returns the current hosting node ID for spawnID, or "" if none.
func (s *Server) liveNodeForSpawn(ctx context.Context, spawnID string) string {
	c, ok, err := s.st.Spawns().LiveContainer(ctx, spawnID)
	if err != nil || !ok {
		return ""
	}
	return c.NodeID
}

// mountsAreOwnerSealed reports whether every non-ephemeral mount of spawnID has an owner-sealed
// ciphertext in the CP store — the condition for a cross-node move without re-delivery.
func (s *Server) mountsAreOwnerSealed(ctx context.Context, spawnID string) bool {
	mounts, err := s.st.Spawns().GetMounts(ctx, spawnID)
	if err != nil {
		return false
	}
	for _, m := range mounts {
		_, gerr := s.journalKeys.Get(ctx, spawnID, journalkey.SecretID(m.Name))
		if errors.Is(gerr, journalkeys.ErrNotFound) {
			return false
		}
		if gerr != nil {
			return false
		}
	}
	return len(mounts) > 0 // true only if there are mounts and all have ciphertexts
}

// noActiveMounts reports whether the spawn has no mounts (no delivery needed).
func noActiveMounts(mounts []store.Mount) bool { return len(mounts) == 0 }
