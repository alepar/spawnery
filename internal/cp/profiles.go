package cp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// ownProfile loads a profile and verifies the caller is its owner. Returns CodeNotFound on
// both missing and owner-mismatch (don't leak existence to other owners).
func (s *Server) ownProfile(ctx context.Context, profileID string) (store.Profile, error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return store.Profile{}, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	p, _, _, err := s.st.Profiles().Get(ctx, profileID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Profile{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("profile not found"))
	}
	if err != nil {
		return store.Profile{}, connect.NewError(connect.CodeInternal, err)
	}
	if p.OwnerID != owner {
		return store.Profile{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("profile not found"))
	}
	return p, nil
}

// mapProfileErr maps store errors to Connect codes for profile mutations.
func mapProfileErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("profile not found"))
	case errors.Is(err, store.ErrConflict):
		return connect.NewError(connect.CodeAborted, fmt.Errorf("version conflict — retry with current version"))
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

// --- CreateProfile ---------------------------------------------------------

func (s *Server) CreateProfile(ctx context.Context, req *connect.Request[cpv1.CreateProfileRequest]) (*connect.Response[cpv1.CreateProfileResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}
	profileID := uuid.NewString()
	now := time.Now().Unix()
	p := store.Profile{
		ProfileID: profileID,
		OwnerID:   owner,
		Name:      name,
		Version:   1,
		UpdatedAt: now,
	}
	if err := s.st.Profiles().Create(ctx, p); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.CreateProfileResponse{
		ProfileId: profileID,
		Version:   1,
	}), nil
}

// --- GetProfile ------------------------------------------------------------

func (s *Server) GetProfile(ctx context.Context, req *connect.Request[cpv1.GetProfileRequest]) (*connect.Response[cpv1.GetProfileResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	p, entries, secrets, err := s.st.Profiles().Get(ctx, req.Msg.ProfileId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("profile not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if p.OwnerID != owner {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("profile not found"))
	}
	profile, err := s.profileToProto(ctx, p, entries, secrets)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.GetProfileResponse{
		Profile: profile,
	}), nil
}

// --- ListProfiles ----------------------------------------------------------

func (s *Server) ListProfiles(ctx context.Context, _ *connect.Request[cpv1.ListProfilesRequest]) (*connect.Response[cpv1.ListProfilesResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	profiles, err := s.st.Profiles().ListByOwner(ctx, owner)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.ProfileSummary, len(profiles))
	for i, p := range profiles {
		out[i] = &cpv1.ProfileSummary{
			ProfileId: p.ProfileID,
			Name:      p.Name,
			Version:   p.Version,
			UpdatedAt: p.UpdatedAt,
		}
	}
	return connect.NewResponse(&cpv1.ListProfilesResponse{Profiles: out}), nil
}

// --- UpdateProfile ---------------------------------------------------------

func (s *Server) UpdateProfile(ctx context.Context, req *connect.Request[cpv1.UpdateProfileRequest]) (*connect.Response[cpv1.UpdateProfileResponse], error) {
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}
	p, err := s.ownProfile(ctx, req.Msg.ProfileId)
	if err != nil {
		return nil, err
	}
	_ = p // ownership verified; caller uses expected_version directly
	newVer, err := s.st.Profiles().Rename(ctx, req.Msg.ProfileId, req.Msg.ExpectedVersion, name, time.Now().Unix())
	if err != nil {
		return nil, mapProfileErr(err)
	}
	return connect.NewResponse(&cpv1.UpdateProfileResponse{Version: newVer}), nil
}

// --- DeleteProfile ---------------------------------------------------------

func (s *Server) DeleteProfile(ctx context.Context, req *connect.Request[cpv1.DeleteProfileRequest]) (*connect.Response[cpv1.DeleteProfileResponse], error) {
	if _, err := s.ownProfile(ctx, req.Msg.ProfileId); err != nil {
		return nil, err
	}
	if err := s.st.Profiles().Delete(ctx, req.Msg.ProfileId); err != nil {
		return nil, mapProfileErr(err)
	}
	return connect.NewResponse(&cpv1.DeleteProfileResponse{}), nil
}

// --- AddProfileEntry -------------------------------------------------------

func (s *Server) AddProfileEntry(ctx context.Context, req *connect.Request[cpv1.AddProfileEntryRequest]) (*connect.Response[cpv1.AddProfileEntryResponse], error) {
	p, err := s.ownProfile(ctx, req.Msg.ProfileId)
	if err != nil {
		return nil, err
	}
	e := req.Msg.Entry
	if e == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("entry is required"))
	}
	// Minimal validation — deep custom-content validation (size/count caps, path confinement)
	// is explicitly deferred to sp-nrzf.3.6.
	if e.Kind == cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("entry kind is required"))
	}
	if e.Source == cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("entry source is required"))
	}
	if strings.TrimSpace(e.Name) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("entry name is required"))
	}
	switch e.Source {
	case cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF:
		if e.CatalogId == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("catalog_id is required for CATALOG_REF source"))
		}
	case cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM:
		// Full validation: name rules, size cap, path confinement (sp-nrzf.3.6).
		if err := validateCustomContent(protoToEntryKind(e.Kind), strings.TrimSpace(e.Name), e.CustomInline); err != nil {
			return nil, err
		}
	case cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF:
		// Shape-only validation here (sp-mwco.1.8 §4.4 design decision 3); bundle existence/
		// ownership/version pin/overrides validation needs DB reads and runs inside the tx below.
		if e.Kind != cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("kind must be SKILL for BUNDLE_REF source"))
		}
		if e.CatalogId != "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("catalog_id must be empty for BUNDLE_REF source"))
		}
		if len(e.CustomInline) != 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("custom_inline must be empty for BUNDLE_REF source"))
		}
		if e.BundleId == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bundle_id is required for BUNDLE_REF source"))
		}
	}

	eid, err := uuid.NewV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("generate entry id: %w", err))
	}
	entryID := eid.String()
	se := store.ProfileEntry{
		ProfileID:     req.Msg.ProfileId,
		EntryID:       entryID,
		Kind:          protoToEntryKind(e.Kind),
		Name:          strings.TrimSpace(e.Name),
		SourceKind:    protoToSourceKind(e.Source),
		CatalogID:     e.CatalogId,
		CustomInline:  e.CustomInline,
		Targets:       e.Targets,
		MCPSecretRefs: e.McpSecretRefs,
	}

	// The catalog-ref existence/visibility check, the entry-count/artifact cap check, the
	// bundle_ref pin/override validation + collision resolution, and the insert all run inside
	// one WithTx so they serialize against a concurrent DeleteCatalogEntry on the same catalog_id
	// (sp-mwco.3.3 §2) — LockRow is the mutex both sides take on the referenced row. Without it,
	// "check the ref exists, then insert" and "check no ref exists, then delete" can interleave
	// and leave a dangling ref on the normal path even though neither side raced an error.
	var newVer uint64
	var warnings []string
	txErr := s.st.WithTx(ctx, func(tx store.Store) error {
		if e.Source == cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF {
			// Tenant gate (sp-mwco.3.4 §4.6 D6): the referenced entry must exist and be visible to
			// the profile's owner (listed OR theirs) — NotFound either way, never
			// PermissionDenied (don't confirm existence). LockRow doubles as the existence check.
			if err := tx.CustomizationCatalog().LockRow(ctx, e.CatalogId); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
				}
				return err
			}
			ce, err := tx.CustomizationCatalog().Get(ctx, e.CatalogId)
			if err != nil {
				return err
			}
			if !catalogEntryVisibleTo(ce, p.OwnerID) {
				return connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
			}
		}

		_, existingEntries, _, err := tx.Profiles().Get(ctx, req.Msg.ProfileId)
		if err != nil {
			return err
		}

		if e.Source == cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF {
			// Ownership (design decision 3): the bundle must exist and belong to this profile's
			// owner — NotFound either way, never leak existence to another owner.
			bundle, err := tx.SkillBundles().Get(ctx, e.BundleId)
			if errors.Is(err, store.ErrNotFound) {
				return connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle not found"))
			}
			if err != nil {
				return err
			}
			if bundle.CreatorID != p.OwnerID {
				return connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle not found"))
			}

			// Pin = latest version at attach (design decision 2). A caller-supplied version_id
			// that isn't the latest is rejected outright — pinning to an arbitrary historical
			// version is out of scope (§4.10).
			latest, err := tx.SkillBundles().LatestVersion(ctx, e.BundleId)
			if errors.Is(err, store.ErrNotFound) {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bundle %q has no ingested version", e.BundleId))
			}
			if err != nil {
				return err
			}
			if e.VersionId != "" && e.VersionId != latest.VersionID {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("pinning to a non-latest version is not supported"))
			}

			members, err := tx.SkillBundles().Members(ctx, latest.VersionID)
			if err != nil {
				return err
			}
			memberSubdirs := make(map[string]bool, len(members))
			for _, m := range members {
				memberSubdirs[m.SourceSubdir] = true
			}

			// Override validation (design decision 4): every exclude/rename key must name a real
			// member — a typo that silently does nothing is worse than an error.
			for _, subdir := range e.ExcludedSubdirs {
				if !memberSubdirs[subdir] {
					return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("exclude: unknown bundle member subdir %q", subdir))
				}
			}
			for subdir, name := range e.MemberRenames {
				if !memberSubdirs[subdir] {
					return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("rename: unknown bundle member subdir %q", subdir))
				}
				if err := validateContentName(name); err != nil {
					return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("rename %q: %w", subdir, err))
				}
				if err := confineDestPath(name); err != nil {
					return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("rename %q: %w", subdir, err))
				}
			}

			excludeSet := toStringSet(e.ExcludedSubdirs)
			remaining := 0
			for _, m := range members {
				if !excludeSet[m.SourceSubdir] {
					remaining++
				}
			}
			if remaining == 0 {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bundle_ref entry excludes every member of the pinned version"))
			}

			se.BundleID = e.BundleId
			se.VersionID = latest.VersionID
			se.ExcludeSubdirs = e.ExcludedSubdirs

			// Attach-time cap (design decision 6 — sp-mwco.1.12's gap, landed here per its bead).
			existingNames, existingCount, err := s.expandedProfileState(ctx, tx, existingEntries, "")
			if err != nil {
				return err
			}
			if err := enforceProfileArtifactCap(existingCount, remaining); err != nil {
				return err
			}

			// Collision = warn-and-rename, never a spawn-creation abort (design decision 5). An
			// explicit user rename that collides is the user's mistake -> InvalidArgument;
			// an implicit (natural-name) collision is auto-resolved with a warning.
			finalRename, w, err := resolveMemberOverrides(members, excludeSet, e.MemberRenames,
				catalogNameFunc(ctx, tx), existingNames, true /* failOnExplicitCollision */)
			if err != nil {
				return err
			}
			warnings = w
			se.RenameSubdirs = finalRename
		} else {
			// Enforce per-profile entry count cap before inserting (sp-nrzf.3.6).
			if err := enforceProfileEntryCap(len(existingEntries)); err != nil {
				return err
			}
		}

		ver, err := tx.Profiles().AddEntry(ctx, req.Msg.ProfileId, req.Msg.ExpectedVersion, se, time.Now().Unix())
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
	return connect.NewResponse(&cpv1.AddProfileEntryResponse{
		EntryId:  entryID,
		Version:  newVer,
		Warnings: warnings,
	}), nil
}

// --- RemoveProfileEntry ----------------------------------------------------

func (s *Server) RemoveProfileEntry(ctx context.Context, req *connect.Request[cpv1.RemoveProfileEntryRequest]) (*connect.Response[cpv1.RemoveProfileEntryResponse], error) {
	if _, err := s.ownProfile(ctx, req.Msg.ProfileId); err != nil {
		return nil, err
	}
	newVer, err := s.st.Profiles().RemoveEntry(ctx, req.Msg.ProfileId, req.Msg.ExpectedVersion, req.Msg.EntryId, time.Now().Unix())
	if err != nil {
		return nil, mapProfileErr(err)
	}
	return connect.NewResponse(&cpv1.RemoveProfileEntryResponse{Version: newVer}), nil
}

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

		newVersion, err := tx.SkillBundles().GetVersion(ctx, req.Msg.VersionId)
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown bundle version %q", req.Msg.VersionId))
		}
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

// --- AddProfileSecretRef ---------------------------------------------------

func (s *Server) AddProfileSecretRef(ctx context.Context, req *connect.Request[cpv1.AddProfileSecretRefRequest]) (*connect.Response[cpv1.AddProfileSecretRefResponse], error) {
	if _, err := s.ownProfile(ctx, req.Msg.ProfileId); err != nil {
		return nil, err
	}
	newVer, err := s.st.Profiles().AddSecretRef(ctx, req.Msg.ProfileId, req.Msg.ExpectedVersion, req.Msg.SecretId, time.Now().Unix())
	if err != nil {
		return nil, mapProfileErr(err)
	}
	return connect.NewResponse(&cpv1.AddProfileSecretRefResponse{Version: newVer}), nil
}

// --- RemoveProfileSecretRef ------------------------------------------------

func (s *Server) RemoveProfileSecretRef(ctx context.Context, req *connect.Request[cpv1.RemoveProfileSecretRefRequest]) (*connect.Response[cpv1.RemoveProfileSecretRefResponse], error) {
	if _, err := s.ownProfile(ctx, req.Msg.ProfileId); err != nil {
		return nil, err
	}
	newVer, err := s.st.Profiles().RemoveSecretRef(ctx, req.Msg.ProfileId, req.Msg.ExpectedVersion, req.Msg.SecretId, time.Now().Unix())
	if err != nil {
		return nil, mapProfileErr(err)
	}
	return connect.NewResponse(&cpv1.RemoveProfileSecretRefResponse{Version: newVer}), nil
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

// --- Wire <-> store conversions --------------------------------------------

// profileToProto builds the full Profile wire message, including the GetProfile-only "update
// available" badge enrichment (design decision 7): pinned_seq/latest_seq/member_count for each
// bundle_ref entry, read via MAX(seq) per bundle — never a GitHub call. ListProfiles stays
// lightweight (ProfileSummary — unenriched) and does not call this.
func (s *Server) profileToProto(ctx context.Context, p store.Profile, entries []store.ProfileEntry, secrets []store.ProfileSecret) (*cpv1.Profile, error) {
	wireEntries := make([]*cpv1.ProfileEntry, len(entries))
	for i, e := range entries {
		pe := &cpv1.ProfileEntry{
			EntryId:       e.EntryID,
			Kind:          entryKindToProto(e.Kind),
			Name:          e.Name,
			Source:        sourceKindToProto(e.SourceKind),
			CatalogId:     e.CatalogID,
			CustomInline:  e.CustomInline,
			Targets:       e.Targets,
			McpSecretRefs: e.MCPSecretRefs,
		}
		if e.SourceKind == store.ProfileSourceBundle {
			pe.BundleId = e.BundleID
			pe.VersionId = e.VersionID
			pe.ExcludedSubdirs = e.ExcludeSubdirs
			pe.MemberRenames = e.RenameSubdirs

			if v, err := s.st.SkillBundles().GetVersion(ctx, e.VersionID); err == nil {
				pe.PinnedSeq = v.Seq
			} else if !errors.Is(err, store.ErrNotFound) {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			if latest, err := s.st.SkillBundles().LatestVersion(ctx, e.BundleID); err == nil {
				pe.LatestSeq = latest.Seq
			} else if !errors.Is(err, store.ErrNotFound) {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			if members, err := s.st.SkillBundles().Members(ctx, e.VersionID); err == nil {
				excludeSet := toStringSet(e.ExcludeSubdirs)
				count := 0
				for _, m := range members {
					if !excludeSet[m.SourceSubdir] {
						count++
					}
				}
				pe.MemberCount = int32(count)
			} else if !errors.Is(err, store.ErrNotFound) {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		}
		wireEntries[i] = pe
	}
	secretIDs := make([]string, len(secrets))
	for i, s := range secrets {
		secretIDs[i] = s.SecretID
	}
	return &cpv1.Profile{
		ProfileId: p.ProfileID,
		Name:      p.Name,
		Version:   p.Version,
		UpdatedAt: p.UpdatedAt,
		Entries:   wireEntries,
		SecretIds: secretIDs,
	}, nil
}

func protoToEntryKind(k cpv1.ProfileEntryKind) store.ProfileEntryKind {
	switch k {
	case cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL:
		return store.ProfileEntrySkill
	case cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP:
		return store.ProfileEntryMCP
	case cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG:
		return store.ProfileEntryConfig
	case cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_PLUGIN:
		return store.ProfileEntryPlugin
	default:
		return ""
	}
}

func entryKindToProto(k store.ProfileEntryKind) cpv1.ProfileEntryKind {
	switch k {
	case store.ProfileEntrySkill:
		return cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL
	case store.ProfileEntryMCP:
		return cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP
	case store.ProfileEntryConfig:
		return cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG
	case store.ProfileEntryPlugin:
		return cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_PLUGIN
	default:
		return cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_UNSPECIFIED
	}
}

func protoToSourceKind(s cpv1.ProfileEntrySource) store.ProfileSourceKind {
	switch s {
	case cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF:
		return store.ProfileSourceCatalog
	case cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM:
		return store.ProfileSourceCustom
	case cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF:
		return store.ProfileSourceBundle
	default:
		return ""
	}
}

func sourceKindToProto(s store.ProfileSourceKind) cpv1.ProfileEntrySource {
	switch s {
	case store.ProfileSourceCatalog:
		return cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF
	case store.ProfileSourceCustom:
		return cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM
	case store.ProfileSourceBundle:
		return cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF
	default:
		return cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_UNSPECIFIED
	}
}
