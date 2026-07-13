package cp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// This file holds the bundle_ref-specific slice of the Profiles RPC surface (sp-mwco.1.8):
// RepinProfileBundle plus the expansion/collision-resolution helpers it shares with
// AddProfileEntry's BUNDLE_REF branch (which stays in profiles.go, alongside the other entry
// sources it's shared with).

// --- RepinProfileBundle (sp-mwco.1.8 §4.9) ---------------------------------

// RepinProfileBundle re-pins a BUNDLE_REF entry onto a newer version of its bundle, gated on the
// caller having viewed the diff first (assertDiffViewed — §4.9's supply-chain gate: there is no
// un-diffed one-click update channel). Exclude/rename overrides are rebased onto the new member
// set; a dropped override for a removed member and an implicit collision auto-rename are both
// reported as warnings, never a hard failure.
func (s *Server) RepinProfileBundle(ctx context.Context, req *connect.Request[cpv1.RepinProfileBundleRequest]) (*connect.Response[cpv1.RepinProfileBundleResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if _, err := s.ownProfile(ctx, req.Msg.ProfileId); err != nil {
		return nil, err
	}

	var newVer uint64
	var warnings []string
	txErr := s.st.WithTx(ctx, func(tx store.Store) error {
		_, entries, _, err := tx.Profiles().Get(ctx, req.Msg.ProfileId)
		if err != nil {
			return err
		}
		var entry *store.ProfileEntry
		for i := range entries {
			if entries[i].EntryID == req.Msg.EntryId {
				entry = &entries[i]
				break
			}
		}
		if entry == nil {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("entry not found"))
		}
		if entry.SourceKind != store.ProfileSourceBundle {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("entry %q is not a bundle_ref entry", req.Msg.EntryId))
		}

		// LockVersion first (mirrors DeleteBundleVersion's LockVersion-before-
		// CountBundleVersionRefs in catalog_delete.go) so this "check ref exists, then insert"
		// can't interleave with a concurrent DeleteBundleVersion's "check no ref exists, then
		// delete". The target version has no profile_entries ref yet — this repin is what
		// creates it — so without the lock DeleteBundleVersion could observe zero refs and
		// delete the version between this read and UpdateEntryPin's write below, leaving a
		// permanently dangling bundle_ref. LockVersion is held for the remainder of this tx, so
		// it also covers the Members(req.Msg.VersionId) read further down.
		if err := tx.SkillBundles().LockVersion(ctx, req.Msg.VersionId); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown bundle version %q", req.Msg.VersionId))
			}
			return err
		}
		newVersion, err := tx.SkillBundles().GetVersion(ctx, req.Msg.VersionId)
		if err != nil {
			return err
		}
		if newVersion.BundleID != entry.BundleID {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("version %q does not belong to bundle %q", req.Msg.VersionId, entry.BundleID))
		}
		pinnedVersion, err := tx.SkillBundles().GetVersion(ctx, entry.VersionID)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		// No rollback (design decision 8): seq must be strictly greater than the pinned seq.
		if newVersion.Seq <= pinnedVersion.Seq {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("version %q (seq %d) is not newer than the pinned version (seq %d)", req.Msg.VersionId, newVersion.Seq, pinnedVersion.Seq))
		}

		// The diff IS the supply-chain gate (§4.9): fails closed on an unknown/unviewed/expired
		// token.
		if err := s.assertDiffViewed(owner, entry.BundleID, req.Msg.VersionId, req.Msg.DiffToken); err != nil {
			return err
		}

		newMembers, err := tx.SkillBundles().Members(ctx, req.Msg.VersionId)
		if err != nil {
			return err
		}
		newSubdirs := make(map[string]bool, len(newMembers))
		for _, m := range newMembers {
			newSubdirs[m.SourceSubdir] = true
		}

		// Rebase overrides onto the new member set: drop keys for members that no longer exist.
		var dropWarnings []string
		newExclude := make([]string, 0, len(entry.ExcludeSubdirs))
		for _, subdir := range entry.ExcludeSubdirs {
			if newSubdirs[subdir] {
				newExclude = append(newExclude, subdir)
			} else {
				dropWarnings = append(dropWarnings, fmt.Sprintf("override for removed member %q dropped", subdir))
			}
		}
		newRename := make(map[string]string, len(entry.RenameSubdirs))
		for subdir, name := range entry.RenameSubdirs {
			if newSubdirs[subdir] {
				newRename[subdir] = name
			} else {
				dropWarnings = append(dropWarnings, fmt.Sprintf("override for removed member %q dropped", subdir))
			}
		}

		excludeSet := toStringSet(newExclude)
		remaining := 0
		for _, m := range newMembers {
			if !excludeSet[m.SourceSubdir] {
				remaining++
			}
		}
		if remaining == 0 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("new bundle version excludes every member after rebasing overrides"))
		}

		existingNames, existingCount, err := s.expandedProfileState(ctx, tx, entries, entry.EntryID)
		if err != nil {
			return err
		}
		if err := enforceProfileArtifactCap(existingCount, remaining); err != nil {
			return err
		}

		// Re-run collision resolution against the rest of the profile. There is no "user" at
		// repin time — every override here is carried forward from a prior attach/repin — so a
		// collision is always auto-resolved with a warning, never a hard failure.
		finalRename, collideWarnings, err := resolveMemberOverrides(newMembers, excludeSet, newRename,
			catalogNameFunc(ctx, tx), existingNames, false /* failOnExplicitCollision */)
		if err != nil {
			return err
		}
		warnings = append(dropWarnings, collideWarnings...)

		overridesJSON, err := store.EncodeBundleOverrides(newExclude, finalRename)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}

		ver, err := tx.Profiles().UpdateEntryPin(ctx, req.Msg.ProfileId, req.Msg.ExpectedVersion, entry.EntryID, req.Msg.VersionId, overridesJSON, time.Now().Unix())
		if err != nil {
			return err
		}
		newVer = ver
		return nil
	})
	if txErr != nil {
		var cerr *connect.Error
		if errors.As(txErr, &cerr) {
			return nil, cerr
		}
		return nil, mapProfileErr(txErr)
	}
	return connect.NewResponse(&cpv1.RepinProfileBundleResponse{
		Version:  newVer,
		Warnings: warnings,
	}), nil
}

// --- Bundle expansion / collision helpers (sp-mwco.1.8 §4.4) --------------

// toStringSet turns a slice into a membership set.
func toStringSet(list []string) map[string]bool {
	set := make(map[string]bool, len(list))
	for _, s := range list {
		set[s] = true
	}
	return set
}

// autoRenameSuffix returns the first "<base>-2", "<base>-3", ... not already claimed in used.
func autoRenameSuffix(base string, used map[string]bool) string {
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if !used[candidate] {
			return candidate
		}
	}
}

// catalogNameFunc returns a lookup closure over tx for a catalog entry's Name, wrapping a
// missing/failed lookup as CodeInternal (the caller's members list came from the same tx, so a
// miss here means DB inconsistency, not caller input).
func catalogNameFunc(ctx context.Context, tx store.Store) func(catalogID string) (string, error) {
	return func(catalogID string) (string, error) {
		ce, err := tx.CustomizationCatalog().Get(ctx, catalogID)
		if err != nil {
			return "", connect.NewError(connect.CodeInternal, fmt.Errorf("get catalog member %q: %w", catalogID, err))
		}
		return ce.Name, nil
	}
}

// expandedEntryNames returns the on-disk skill-dir name(s) one entry contributes: exactly
// entry.Name for catalog_ref/custom, or one post-exclude/rename name per bundle member for
// bundle_ref.
func expandedEntryNames(ctx context.Context, tx store.Store, entry store.ProfileEntry) ([]string, error) {
	if entry.SourceKind != store.ProfileSourceBundle {
		return []string{entry.Name}, nil
	}
	members, err := tx.SkillBundles().Members(ctx, entry.VersionID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("entry %q: members: %w", entry.EntryID, err))
	}
	excludeSet := toStringSet(entry.ExcludeSubdirs)
	getName := catalogNameFunc(ctx, tx)
	names := make([]string, 0, len(members))
	for _, m := range members {
		if excludeSet[m.SourceSubdir] {
			continue
		}
		if name, ok := entry.RenameSubdirs[m.SourceSubdir]; ok {
			names = append(names, name)
			continue
		}
		name, err := getName(m.CatalogID)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

// expandedProfileState computes the profile's current expanded skill-dir-name set and total
// expanded artifact count across entries, skipping skipEntryID if non-empty (RepinProfileBundle
// recomputes against "the rest of the profile", excluding the entry being repinned). Both attach
// (AddProfileEntry) and re-pin (RepinProfileBundle) share this for cap enforcement and collision
// resolution (design decisions 5/6/8).
func (s *Server) expandedProfileState(ctx context.Context, tx store.Store, entries []store.ProfileEntry, skipEntryID string) (names map[string]bool, count int, err error) {
	names = make(map[string]bool)
	for _, entry := range entries {
		if skipEntryID != "" && entry.EntryID == skipEntryID {
			continue
		}
		entryNames, err := expandedEntryNames(ctx, tx, entry)
		if err != nil {
			return nil, 0, err
		}
		for _, n := range entryNames {
			names[n] = true
		}
		count += len(entryNames)
	}
	return names, count, nil
}

// resolveMemberOverrides walks members in position order (skipping excludeSet), returning the
// final rename map plus warnings for any collision that had to be auto-resolved. usedNames is
// seeded with the profile's other expanded names and mutated in place as each surviving member
// claims its name. failOnExplicitCollision distinguishes attach's fresh user input (a colliding
// explicit rename is the user's mistake -> InvalidArgument) from re-pin's carried-forward
// overrides (never fail; always auto-rename with a warning — there is no "user" at repin time,
// just a rebase of previously-made choices).
func resolveMemberOverrides(
	members []store.SkillBundleMember,
	excludeSet map[string]bool,
	renameMap map[string]string,
	catalogName func(catalogID string) (string, error),
	usedNames map[string]bool,
	failOnExplicitCollision bool,
) (finalRename map[string]string, warnings []string, err error) {
	finalRename = make(map[string]string)
	for _, m := range members {
		if excludeSet[m.SourceSubdir] {
			continue
		}
		if name, explicit := renameMap[m.SourceSubdir]; explicit {
			if usedNames[name] {
				if failOnExplicitCollision {
					return nil, nil, connect.NewError(connect.CodeInvalidArgument,
						fmt.Errorf("rename %q for bundle member %q collides with another profile entry", name, m.SourceSubdir))
				}
				candidate := autoRenameSuffix(name, usedNames)
				finalRename[m.SourceSubdir] = candidate
				usedNames[candidate] = true
				warnings = append(warnings, fmt.Sprintf(
					"bundle member %q renamed to %q (name already used by another profile entry)", name, candidate))
				continue
			}
			finalRename[m.SourceSubdir] = name
			usedNames[name] = true
			continue
		}
		natural, err := catalogName(m.CatalogID)
		if err != nil {
			return nil, nil, err
		}
		if usedNames[natural] {
			candidate := autoRenameSuffix(natural, usedNames)
			finalRename[m.SourceSubdir] = candidate
			usedNames[candidate] = true
			warnings = append(warnings, fmt.Sprintf(
				"bundle member %q renamed to %q (name already used by another profile entry)", natural, candidate))
			continue
		}
		usedNames[natural] = true
	}
	return finalRename, warnings, nil
}
