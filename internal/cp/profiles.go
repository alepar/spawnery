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
	return connect.NewResponse(&cpv1.GetProfileResponse{
		Profile: profileToProto(p, entries, secrets),
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
	if _, err := s.ownProfile(ctx, req.Msg.ProfileId); err != nil {
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
	}

	// Enforce per-profile entry count cap before inserting (sp-nrzf.3.6).
	_, existingEntries, _, err := s.st.Profiles().Get(ctx, req.Msg.ProfileId)
	if err != nil {
		return nil, mapProfileErr(err)
	}
	if err := enforceProfileEntryCap(len(existingEntries)); err != nil {
		return nil, err
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
	newVer, err := s.st.Profiles().AddEntry(ctx, req.Msg.ProfileId, req.Msg.ExpectedVersion, se, time.Now().Unix())
	if err != nil {
		return nil, mapProfileErr(err)
	}
	return connect.NewResponse(&cpv1.AddProfileEntryResponse{
		EntryId: entryID,
		Version: newVer,
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

func profileToProto(p store.Profile, entries []store.ProfileEntry, secrets []store.ProfileSecret) *cpv1.Profile {
	wireEntries := make([]*cpv1.ProfileEntry, len(entries))
	for i, e := range entries {
		wireEntries[i] = &cpv1.ProfileEntry{
			EntryId:       e.EntryID,
			Kind:          entryKindToProto(e.Kind),
			Name:          e.Name,
			Source:        sourceKindToProto(e.SourceKind),
			CatalogId:     e.CatalogID,
			CustomInline:  e.CustomInline,
			Targets:       e.Targets,
			McpSecretRefs: e.MCPSecretRefs,
		}
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
	}
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
	default:
		return cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_UNSPECIFIED
	}
}
