package cp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// --- CreateCatalogEntry -------------------------------------------------------

func (s *Server) CreateCatalogEntry(ctx context.Context, req *connect.Request[cpv1.CreateCatalogEntryRequest]) (*connect.Response[cpv1.CreateCatalogEntryResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if req.Msg.Kind == cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("kind is required"))
	}
	name := strings.TrimSpace(req.Msg.Name)
	if err := validateCustomContent(protoToEntryKind(req.Msg.Kind), name, req.Msg.Content); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	catalogID := uuid.NewString()
	e := store.CustomizationCatalogEntry{
		CatalogID:   catalogID,
		CreatorID:   owner,
		Kind:        string(protoToEntryKind(req.Msg.Kind)),
		Name:        name,
		Description: req.Msg.Description,
		Content:     req.Msg.Content,
		// Unlisted by default (sp-mwco.3.4 §4.6 D1): ListVisibleTo has no tenant filter beyond
		// "listed OR mine", so an inline entry left listed=true here would leak to every other
		// tenant just like a URL-ingested skill. PublishCatalogEntry (admin-only) is the sole
		// door onto the global catalog.
		Listed:    false,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.st.CustomizationCatalog().Create(ctx, e); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.CreateCatalogEntryResponse{CatalogId: catalogID}), nil
}

// --- GetCatalogEntry ---------------------------------------------------------

func (s *Server) GetCatalogEntry(ctx context.Context, req *connect.Request[cpv1.GetCatalogEntryRequest]) (*connect.Response[cpv1.GetCatalogEntryResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	e, err := s.st.CustomizationCatalog().Get(ctx, req.Msg.CatalogId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Tenant gate (sp-mwco.3.4 §4.6 D6): NotFound, not PermissionDenied — do not confirm
	// existence of an entry the caller can't see.
	if !catalogEntryVisibleTo(e, owner) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
	}
	return connect.NewResponse(&cpv1.GetCatalogEntryResponse{Entry: catalogEntryToProto(e)}), nil
}

// --- ListCatalogEntries -------------------------------------------------------

func (s *Server) ListCatalogEntries(ctx context.Context, _ *connect.Request[cpv1.ListCatalogEntriesRequest]) (*connect.Response[cpv1.ListCatalogEntriesResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	entries, err := s.st.CustomizationCatalog().ListVisibleTo(ctx, owner)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.CatalogEntrySummary, len(entries))
	for i, e := range entries {
		out[i] = &cpv1.CatalogEntrySummary{
			CatalogId:   e.CatalogID,
			Kind:        entryKindToProto(store.ProfileEntryKind(e.Kind)),
			Name:        e.Name,
			Description: e.Description,
		}
	}
	return connect.NewResponse(&cpv1.ListCatalogEntriesResponse{Entries: out}), nil
}

// --- UpdateCatalogEntry -------------------------------------------------------

func (s *Server) UpdateCatalogEntry(ctx context.Context, req *connect.Request[cpv1.UpdateCatalogEntryRequest]) (*connect.Response[cpv1.UpdateCatalogEntryResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	e, err := s.st.CustomizationCatalog().Get(ctx, req.Msg.CatalogId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if e.CreatorID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.CatalogId))
	}
	name := strings.TrimSpace(req.Msg.Name)
	if err := validateCustomContent(store.ProfileEntryKind(e.Kind), name, req.Msg.Content); err != nil {
		return nil, err
	}
	if err := s.st.CustomizationCatalog().Update(ctx, req.Msg.CatalogId, name, req.Msg.Description, req.Msg.Content, time.Now().Unix()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.UpdateCatalogEntryResponse{}), nil
}

// --- DeleteCatalogEntry -------------------------------------------------------

func (s *Server) DeleteCatalogEntry(ctx context.Context, req *connect.Request[cpv1.DeleteCatalogEntryRequest]) (*connect.Response[cpv1.DeleteCatalogEntryResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	e, err := s.st.CustomizationCatalog().Get(ctx, req.Msg.CatalogId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if e.CreatorID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.CatalogId))
	}
	// Resolve affected spawns BEFORE deleting — no FK cascade from catalog→profile_entries,
	// so the entries survive the catalog delete, but capturing first is belt-and-suspenders.
	affected, killErr := s.resolveAffectedSpawns(ctx, req.Msg.CatalogId)
	if killErr != nil {
		log.Printf("kill-switch: resolve for catalog %s failed: %v (delete proceeds)", req.Msg.CatalogId, killErr)
	}
	if err := s.st.CustomizationCatalog().Delete(ctx, req.Msg.CatalogId); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	s.killSwitchForCatalog(ctx, req.Msg.CatalogId, affected)
	return connect.NewResponse(&cpv1.DeleteCatalogEntryResponse{}), nil
}

// --- SetCatalogListing -------------------------------------------------------

func (s *Server) SetCatalogListing(ctx context.Context, req *connect.Request[cpv1.SetCatalogListingRequest]) (*connect.Response[cpv1.SetCatalogListingResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	e, err := s.st.CustomizationCatalog().Get(ctx, req.Msg.CatalogId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if req.Msg.Listed {
		// Only an admin may (re)list via the legacy verb (sp-mwco.3.4 §4.6 D4) — otherwise it is a
		// trivial bypass of the admin-only PublishCatalogEntry gate. Creators keep unlisting, below.
		if !s.isAdmin(owner) {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("listing an entry requires admin"))
		}
		if err := s.st.CustomizationCatalog().SetListed(ctx, req.Msg.CatalogId, true); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		return connect.NewResponse(&cpv1.SetCatalogListingResponse{}), nil
	}

	if e.CreatorID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.CatalogId))
	}
	counts, err := s.unlistWithGuard(ctx, req.Msg.CatalogId, req.Msg.Confirm)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.SetCatalogListingResponse{
		ReferencedProfiles:       int32(counts.profiles),
		ReferencedOwners:         int32(counts.owners),
		ReferencedBundleVersions: int32(counts.bundleVersions),
		TerminatedSpawns:         int32(counts.terminated),
	}), nil
}

// listingCounts summarizes the reference blast-radius of an unlist — used both to build the
// guard's FailedPrecondition message and to populate the wire response after a confirmed unlist.
type listingCounts struct {
	profiles       int
	owners         int
	bundleVersions int
	terminated     int
}

// unlistWithGuard is the shared listed=false path behind SetCatalogListing and
// UnpublishCatalogEntry (sp-mwco.3.4 §4.6 D5 — guarded unlisting). It counts references FIRST:
// profiles/owners referencing catalogID via a catalog_ref entry, and bundle versions that include
// it as a member. A nonzero reference count with confirm=false is rejected with
// FailedPrecondition carrying COUNTS ONLY — never profile ids, owner ids, or spawn ids (the same
// cross-tenant-disclosure rule as the delete path, sp-mwco.3.3 §4.3). confirm=true (or zero
// references) proceeds to SetListed(false) and the existing kill-switch.
func (s *Server) unlistWithGuard(ctx context.Context, catalogID string, confirm bool) (listingCounts, error) {
	if _, err := s.st.CustomizationCatalog().Get(ctx, catalogID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return listingCounts{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
		}
		return listingCounts{}, connect.NewError(connect.CodeInternal, err)
	}

	profiles, owners, err := s.st.Profiles().CountRefsByCatalogRef(ctx, catalogID)
	if err != nil {
		return listingCounts{}, connect.NewError(connect.CodeInternal, err)
	}
	versionIDs, err := s.st.SkillBundles().MemberVersionIDs(ctx, catalogID)
	if err != nil {
		return listingCounts{}, connect.NewError(connect.CodeInternal, err)
	}
	counts := listingCounts{profiles: profiles, owners: owners, bundleVersions: len(versionIDs)}

	// Only a profile catalog_ref (not bare bundle-version membership) can put a spawn in the
	// blast radius — resolving spawns for an unreferenced entry would always come back empty.
	var affected []store.Spawn
	if profiles > 0 {
		var killErr error
		affected, killErr = s.resolveAffectedSpawns(ctx, catalogID)
		if killErr != nil {
			log.Printf("kill-switch: resolve for catalog %s failed: %v", catalogID, killErr)
		}
	}

	if (profiles > 0 || counts.bundleVersions > 0) && !confirm {
		return listingCounts{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"entry is referenced by %d profile(s) across %d owner(s) and %d bundle version(s); re-send with confirm=true to unlist (this terminates %d running spawn(s))",
			profiles, owners, counts.bundleVersions, len(affected)))
	}

	if err := s.st.CustomizationCatalog().SetListed(ctx, catalogID, false); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return listingCounts{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
		}
		return listingCounts{}, connect.NewError(connect.CodeInternal, err)
	}
	s.killSwitchForCatalog(ctx, catalogID, affected)
	counts.terminated = len(affected)
	return counts, nil
}

// --- Kill-switch helpers (sp-nrzf.3.9) ----------------------------------------

// resolveAffectedSpawns returns the live (non-deleted) spawns that reference catalogID
// through a catalog_ref profile entry. Returns (nil, err) on store failure.
func (s *Server) resolveAffectedSpawns(ctx context.Context, catalogID string) ([]store.Spawn, error) {
	profileIDs, err := s.st.Profiles().ListProfileIDsByCatalogRef(ctx, catalogID)
	if err != nil {
		return nil, fmt.Errorf("list profile ids: %w", err)
	}
	spawns, err := s.st.Spawns().ListLiveByProfileIDs(ctx, profileIDs)
	if err != nil {
		return nil, fmt.Errorf("list live spawns: %w", err)
	}
	return spawns, nil
}

// killSwitchForCatalog terminates all affected spawns on a best-effort basis.
// Per-spawn errors are logged and do not abort subsequent terminations. A summary
// log line is emitted regardless. The security goal is: the entry is revoked AND we
// attempt to stop all referencing spawns; a transiently unreachable node must not block
// the revoke itself (caller has already committed the Delete/SetListed change).
func (s *Server) killSwitchForCatalog(ctx context.Context, catalogID string, affected []store.Spawn) {
	terminated := 0
	for _, sp := range affected {
		reason := "catalog_revoke:" + catalogID
		if err := s.terminateSpawn(ctx, sp.ID, reason); err != nil {
			log.Printf("kill-switch: catalog %s: failed to terminate spawn %s: %v", catalogID, sp.ID, err)
			continue
		}
		terminated++
	}
	log.Printf("kill-switch: catalog %s: terminated %d/%d affected spawns", catalogID, terminated, len(affected))
}

// catalogEntryVisibleTo reports whether a catalog entry is visible to owner under the tenant
// gate (sp-mwco.3.4 §4.6 D6): listed OR the caller is the creator.
func catalogEntryVisibleTo(e store.CustomizationCatalogEntry, owner string) bool {
	return e.Listed || e.CreatorID == owner
}

// --- Wire <-> store conversions -----------------------------------------------

func catalogEntryToProto(e store.CustomizationCatalogEntry) *cpv1.CustomizationCatalogEntry {
	return &cpv1.CustomizationCatalogEntry{
		CatalogId:   e.CatalogID,
		CreatorId:   e.CreatorID,
		Kind:        entryKindToProto(store.ProfileEntryKind(e.Kind)),
		Name:        e.Name,
		Description: e.Description,
		Content:     e.Content,
		Listed:      e.Listed,
		CreatedAt:   e.CreatedAt,
		UpdatedAt:   e.UpdatedAt,
	}
}
