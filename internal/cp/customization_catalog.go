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
		Listed:      true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.st.CustomizationCatalog().Create(ctx, e); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.CreateCatalogEntryResponse{CatalogId: catalogID}), nil
}

// --- GetCatalogEntry ---------------------------------------------------------

func (s *Server) GetCatalogEntry(ctx context.Context, req *connect.Request[cpv1.GetCatalogEntryRequest]) (*connect.Response[cpv1.GetCatalogEntryResponse], error) {
	_, ok := auth.OwnerFromContext(ctx)
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
	return connect.NewResponse(&cpv1.GetCatalogEntryResponse{Entry: catalogEntryToProto(e)}), nil
}

// --- ListCatalogEntries -------------------------------------------------------

func (s *Server) ListCatalogEntries(ctx context.Context, _ *connect.Request[cpv1.ListCatalogEntriesRequest]) (*connect.Response[cpv1.ListCatalogEntriesResponse], error) {
	_, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	entries, err := s.st.CustomizationCatalog().List(ctx)
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
	if err := s.st.CustomizationCatalog().Delete(ctx, req.Msg.CatalogId); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
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
	if e.CreatorID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.CatalogId))
	}
	if err := s.st.CustomizationCatalog().SetListed(ctx, req.Msg.CatalogId, req.Msg.Listed); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.SetCatalogListingResponse{}), nil
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
