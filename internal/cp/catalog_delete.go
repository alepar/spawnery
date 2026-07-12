package cp

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// This file holds the safe-delete machinery for the customization catalog and skill bundles
// (sp-mwco.3.3 §4.3): the counts-only reference-check error builders shared with
// DeleteCatalogEntry (customization_catalog.go), and the DeleteBundle/DeleteBundleVersion
// handlers with their own reference check + kill-switch resolution.
//
// Bundle attach does not exist on the wire yet (proto ProfileEntrySource has no BUNDLE_REF —
// sp-mwco.1's follow-on slice), so the bundle-side kill-switch cannot reuse
// customization_catalog.go's resolveAffectedSpawns (catalog_ref-only, and OWNED by sp-mwco.1.6's
// bundle-aware rewrite — not touched here). resolveAffectedSpawnsByBundle/ByVersion below are a
// LOCAL, bundle_ref-only equivalent for this task only; when sp-mwco.1.6 lands its bundle-aware
// resolveAffectedSpawns, collapse these into it (tracked as a follow-up).

// refusedByProfileRefs builds the counts-only FailedPrecondition error shared by
// DeleteCatalogEntry, DeleteBundle, and DeleteBundleVersion. subject identifies what's being
// deleted, e.g. "catalog entry <id>", "bundle <id>", "bundle version <id>". COUNTS ONLY — never a
// profile id, name, or owner id: catalog entries and bundles are global and their refs span
// tenants, so naming a referencing profile/owner in the error would be a cross-tenant disclosure.
func refusedByProfileRefs(subject string, profiles, owners int) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
		"%s is referenced by %d profile(s) across %d owner(s); re-run with force=true to delete anyway",
		subject, profiles, owners))
}

// refusedByBundleMembership builds the FailedPrecondition error for deleting a catalog entry that
// is a member of one or more bundle versions. force does NOT override this check (forcing would
// orphan a live bundle version) — the caller must delete the bundle version instead.
func refusedByBundleMembership(catalogID string, versionCount int) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
		"catalog entry %s is a member of %d bundle version(s); delete the bundle version instead (force is not honored for bundle members)",
		catalogID, versionCount))
}

// --- DeleteBundle -------------------------------------------------------------

// DeleteBundle deletes a skill bundle, creator-gated and guarded by the same counts-only
// reference-check + lock pattern as DeleteCatalogEntry (sp-mwco.3.3 §3), scoped to bundle_ref
// profile entries pinned to this bundle (any version). FK ON DELETE CASCADE removes the bundle's
// versions and their member rows, but NOT the member customization_catalog rows themselves —
// content-identity dedup means a catalog row can be shared across bundles/versions, so deleting it
// here could silently break another bundle's membership. Orphaned member rows are left unlisted;
// revocation is the sha denylist (§4.2), not deletion.
func (s *Server) DeleteBundle(ctx context.Context, req *connect.Request[cpv1.DeleteBundleRequest]) (*connect.Response[cpv1.DeleteBundleResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	b, err := s.st.SkillBundles().Get(ctx, req.Msg.BundleId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if b.CreatorID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.BundleId))
	}

	var affected []store.Spawn
	txErr := s.st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.SkillBundles().LockBundle(ctx, req.Msg.BundleId); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle not found"))
			}
			return err
		}
		b, err := tx.SkillBundles().Get(ctx, req.Msg.BundleId)
		if err != nil {
			return err
		}
		if b.CreatorID != owner {
			return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.BundleId))
		}

		profiles, owners, err := tx.Profiles().CountBundleRefs(ctx, req.Msg.BundleId)
		if err != nil {
			return err
		}
		if profiles > 0 && !req.Msg.Force {
			return refusedByProfileRefs(fmt.Sprintf("bundle %s", req.Msg.BundleId), profiles, owners)
		}

		var resolveErr error
		affected, resolveErr = resolveAffectedSpawnsByBundle(ctx, tx, req.Msg.BundleId)
		if resolveErr != nil {
			return resolveErr
		}
		return tx.SkillBundles().DeleteBundle(ctx, req.Msg.BundleId)
	})
	if txErr != nil {
		var cerr *connect.Error
		if errors.As(txErr, &cerr) {
			return nil, cerr
		}
		return nil, connect.NewError(connect.CodeInternal, txErr)
	}

	s.killSwitchForCatalog(ctx, req.Msg.BundleId, affected)
	return connect.NewResponse(&cpv1.DeleteBundleResponse{}), nil
}

// --- DeleteBundleVersion --------------------------------------------------------

// DeleteBundleVersion deletes one version of a bundle, creator-gated (via the owning bundle) and
// guarded the same way as DeleteBundle, scoped to bundle_ref profile entries pinned to this exact
// version. Sibling versions of the same bundle, and the version's member catalog rows, are
// untouched.
func (s *Server) DeleteBundleVersion(ctx context.Context, req *connect.Request[cpv1.DeleteBundleVersionRequest]) (*connect.Response[cpv1.DeleteBundleVersionResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	v, err := s.st.SkillBundles().GetVersion(ctx, req.Msg.VersionId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle version not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	b, err := s.st.SkillBundles().Get(ctx, v.BundleID)
	if errors.Is(err, store.ErrNotFound) {
		// Narrow race: a concurrent DeleteBundle cascaded the version away between GetVersion and
		// here. This is only the fast pre-tx path; report NotFound rather than Internal.
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle version not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if b.CreatorID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.VersionId))
	}

	var affected []store.Spawn
	txErr := s.st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.SkillBundles().LockVersion(ctx, req.Msg.VersionId); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle version not found"))
			}
			return err
		}
		v, err := tx.SkillBundles().GetVersion(ctx, req.Msg.VersionId)
		if err != nil {
			return err
		}
		b, err := tx.SkillBundles().Get(ctx, v.BundleID)
		if err != nil {
			return err
		}
		if b.CreatorID != owner {
			return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.VersionId))
		}

		profiles, owners, err := tx.Profiles().CountBundleVersionRefs(ctx, req.Msg.VersionId)
		if err != nil {
			return err
		}
		if profiles > 0 && !req.Msg.Force {
			return refusedByProfileRefs(fmt.Sprintf("bundle version %s", req.Msg.VersionId), profiles, owners)
		}

		var resolveErr error
		affected, resolveErr = resolveAffectedSpawnsByBundleVersion(ctx, tx, req.Msg.VersionId)
		if resolveErr != nil {
			return resolveErr
		}
		return tx.SkillBundles().DeleteBundleVersion(ctx, req.Msg.VersionId)
	})
	if txErr != nil {
		var cerr *connect.Error
		if errors.As(txErr, &cerr) {
			return nil, cerr
		}
		return nil, connect.NewError(connect.CodeInternal, txErr)
	}

	s.killSwitchForCatalog(ctx, req.Msg.VersionId, affected)
	return connect.NewResponse(&cpv1.DeleteBundleVersionResponse{}), nil
}

// --- Local bundle-aware spawn resolution (sp-mwco.3.3, until sp-mwco.1.6 lands) ----------------

// resolveAffectedSpawnsByBundle returns the live (non-deleted) spawns whose profile contains a
// bundle_ref entry pinned to bundleID (any version). LOCAL to this task: sp-mwco.1.6 owns
// rewriting customization_catalog.go's resolveAffectedSpawns to be bundle-aware; when it lands,
// collapse this into it (follow-up).
func resolveAffectedSpawnsByBundle(ctx context.Context, tx store.Store, bundleID string) ([]store.Spawn, error) {
	profileIDs, err := tx.Profiles().ListProfileIDsByBundleRef(ctx, bundleID)
	if err != nil {
		return nil, fmt.Errorf("list profile ids: %w", err)
	}
	spawns, err := tx.Spawns().ListLiveByProfileIDs(ctx, profileIDs)
	if err != nil {
		return nil, fmt.Errorf("list live spawns: %w", err)
	}
	return spawns, nil
}

// resolveAffectedSpawnsByBundleVersion is resolveAffectedSpawnsByBundle scoped to one version.
func resolveAffectedSpawnsByBundleVersion(ctx context.Context, tx store.Store, versionID string) ([]store.Spawn, error) {
	profileIDs, err := tx.Profiles().ListProfileIDsByBundleVersionRef(ctx, versionID)
	if err != nil {
		return nil, fmt.Errorf("list profile ids: %w", err)
	}
	spawns, err := tx.Spawns().ListLiveByProfileIDs(ctx, profileIDs)
	if err != nil {
		return nil, fmt.Errorf("list live spawns: %w", err)
	}
	return spawns, nil
}
