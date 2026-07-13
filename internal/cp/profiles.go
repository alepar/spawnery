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
			// owner — NotFound either way, never leak existence to another owner. LockBundle
			// first (mirrors DeleteBundle's LockBundle-before-CountBundleRefs in
			// catalog_delete.go) so this "check ref exists, then insert" can't interleave with a
			// concurrent DeleteBundle's "check no ref exists, then delete": without the lock,
			// DeleteBundle could observe zero refs and cascade-delete the bundle/version/members
			// between this read and AddEntry's insert below, leaving a permanently dangling
			// bundle_ref that GetProfile's badge silently zeroes and CreateSpawn hard-fails on.
			if err := tx.SkillBundles().LockBundle(ctx, e.BundleId); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle not found"))
				}
				return err
			}
			bundle, err := tx.SkillBundles().Get(ctx, e.BundleId)
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
