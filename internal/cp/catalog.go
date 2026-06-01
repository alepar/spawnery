package cp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

func tierToProto(t store.Tier) cpv1.TrustTier {
	switch t {
	case store.TierUnverified:
		return cpv1.TrustTier_TRUST_TIER_UNVERIFIED
	case store.TierScanned:
		return cpv1.TrustTier_TRUST_TIER_SCANNED
	case store.TierReviewed:
		return cpv1.TrustTier_TRUST_TIER_REVIEWED
	default:
		return cpv1.TrustTier_TRUST_TIER_UNSPECIFIED
	}
}

func splitTags(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ListApps returns the public, listed catalog (optionally filtered by query). Browsing requires an
// authenticated caller but is NOT owner-scoped — the catalog is public.
func (s *Server) ListApps(ctx context.Context, req *connect.Request[cpv1.ListAppsRequest]) (*connect.Response[cpv1.ListAppsResponse], error) {
	if _, ok := auth.OwnerFromContext(ctx); !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	entries, err := s.st.Apps().Catalog(ctx, store.CatalogFilter{Query: req.Msg.Query})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.AppSummary, len(entries))
	for i, e := range entries {
		out[i] = catalogEntryToSummary(e)
	}
	return connect.NewResponse(&cpv1.ListAppsResponse{Apps: out}), nil
}

// catalogEntryToSummary maps a store CatalogEntry to the wire AppSummary.
func catalogEntryToSummary(e store.CatalogEntry) *cpv1.AppSummary {
	return &cpv1.AppSummary{
		Id: e.App.ID, DisplayName: e.App.DisplayName, Summary: e.App.Summary,
		Tags: splitTags(e.App.Tags), LatestVersion: e.LatestVersion, LatestTier: tierToProto(e.LatestTier),
		Listed: e.App.Listed,
	}
}

// ListMyApps returns the authenticated owner's apps (including unlisted/taken-down) for management.
func (s *Server) ListMyApps(ctx context.Context, _ *connect.Request[cpv1.ListMyAppsRequest]) (*connect.Response[cpv1.ListMyAppsResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	entries, err := s.st.Apps().ListByCreator(ctx, owner)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.AppSummary, len(entries))
	for i, e := range entries {
		out[i] = catalogEntryToSummary(e)
	}
	return connect.NewResponse(&cpv1.ListMyAppsResponse{Apps: out}), nil
}

// GetApp returns one catalog app's metadata + its versions (newest first).
func (s *Server) GetApp(ctx context.Context, req *connect.Request[cpv1.GetAppRequest]) (*connect.Response[cpv1.GetAppResponse], error) {
	if _, ok := auth.OwnerFromContext(ctx); !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	app, versions, err := s.st.Apps().AppDetail(ctx, req.Msg.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	summary := &cpv1.AppSummary{
		Id: app.ID, DisplayName: app.DisplayName, Summary: app.Summary, Tags: splitTags(app.Tags),
		Listed: app.Listed,
	}
	vout := make([]*cpv1.AppVersionSummary, len(versions))
	for i, v := range versions {
		vout[i] = &cpv1.AppVersionSummary{Version: v.Version, Ref: v.Ref, Tier: tierToProto(v.Tier), CreatedAt: v.CreatedAt}
		if i == 0 {
			summary.LatestVersion, summary.LatestTier = v.Version, tierToProto(v.Tier)
		}
	}
	resp := &cpv1.GetAppResponse{App: summary, Versions: vout}
	if len(versions) > 0 && versions[0].Manifest != "" {
		var m cpv1.AppManifest
		if err := protojson.Unmarshal([]byte(versions[0].Manifest), &m); err != nil {
			log.Printf("GetApp %s: manifest parse: %v", req.Msg.Id, err) // non-fatal
		} else {
			resp.Manifest = &m
		}
	}
	return connect.NewResponse(resp), nil
}
