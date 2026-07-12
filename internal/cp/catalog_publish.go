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

// isAdmin reports whether owner is present in the admin allowlist (sp-mwco.3.4 §4.6 D3). Fails
// closed: a nil/empty allowlist means isAdmin is always false.
func (s *Server) isAdmin(owner string) bool {
	_, ok := s.adminOwners[owner]
	return ok
}

// requireAdmin verifies the caller is authenticated AND admin-allowlisted. The admin check runs
// BEFORE any existence lookup: a non-admin must not be able to use NotFound-vs-PermissionDenied
// to probe whether a catalog/bundle id exists.
func (s *Server) requireAdmin(ctx context.Context) (string, error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return "", connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if !s.isAdmin(owner) {
		return "", connect.NewError(connect.CodePermissionDenied, fmt.Errorf("admin only"))
	}
	return owner, nil
}

// --- PublishCatalogEntry -------------------------------------------------------

// PublishCatalogEntry lists a catalog entry into the global catalog (admin-only). This is the
// sole door through which a row created unlisted (sp-mwco.3.4 §4.6 D1) becomes visible to every
// tenant.
func (s *Server) PublishCatalogEntry(ctx context.Context, req *connect.Request[cpv1.PublishCatalogEntryRequest]) (*connect.Response[cpv1.PublishCatalogEntryResponse], error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := s.st.CustomizationCatalog().SetListed(ctx, req.Msg.CatalogId, true); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.PublishCatalogEntryResponse{}), nil
}

// --- UnpublishCatalogEntry -------------------------------------------------------

// UnpublishCatalogEntry is the admin path to listed=false — it shares SetCatalogListing(listed=
// false)'s guarded-unlist semantics (reference-count confirmation + kill-switch) via
// unlistWithGuard, gated to admins instead of the creator.
func (s *Server) UnpublishCatalogEntry(ctx context.Context, req *connect.Request[cpv1.UnpublishCatalogEntryRequest]) (*connect.Response[cpv1.UnpublishCatalogEntryResponse], error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	counts, err := s.unlistWithGuard(ctx, req.Msg.CatalogId, req.Msg.Confirm)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.UnpublishCatalogEntryResponse{
		ReferencedProfiles:       int32(counts.profiles),
		ReferencedOwners:         int32(counts.owners),
		ReferencedBundleVersions: int32(counts.bundleVersions),
		TerminatedSpawns:         int32(counts.terminated),
	}), nil
}

// --- PublishBundle -------------------------------------------------------

// PublishBundle publishes every member of every version of a bundle (admin-only) — not just the
// latest, since a profile may pin an older version and must still be able to resolve it. Not
// transactional: SetListed is idempotent, so a partial failure here (e.g. a node hiccup mid-loop)
// is safely retryable by calling PublishBundle again.
func (s *Server) PublishBundle(ctx context.Context, req *connect.Request[cpv1.PublishBundleRequest]) (*connect.Response[cpv1.PublishBundleResponse], error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if _, err := s.st.SkillBundles().Get(ctx, req.Msg.BundleId); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	versions, err := s.st.SkillBundles().ListVersions(ctx, req.Msg.BundleId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	for _, v := range versions {
		members, err := s.st.SkillBundles().Members(ctx, v.VersionID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, m := range members {
			if err := s.st.CustomizationCatalog().SetListed(ctx, m.CatalogID, true); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("publish bundle member %s: %w", m.CatalogID, err))
			}
		}
	}
	return connect.NewResponse(&cpv1.PublishBundleResponse{}), nil
}
