package cp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// RegisterAppVersion registers (or updates) an app version from a structured manifest — the
// registration source of truth. Fresh versions enter at tier `unverified`. CP does not fetch the
// definition repo; CI maps spawneryapp.yml -> this API.
func (s *Server) RegisterAppVersion(ctx context.Context, req *connect.Request[cpv1.RegisterAppVersionRequest]) (*connect.Response[cpv1.RegisterAppVersionResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	m := req.Msg.Manifest
	if err := validateManifest(m, req.Msg.Version, req.Msg.Ref); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	creator := owner
	switch existing, err := s.st.Apps().Creator(ctx, m.Id); {
	case err == nil:
		if existing != owner {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", m.Id))
		}
		creator = existing
	case errors.Is(err, store.ErrNotFound):
		// new app; owner becomes creator
	default:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	blob, err := protojson.Marshal(m)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	now := time.Now().Unix()
	mounts := make([]store.MountDecl, len(m.Mounts))
	for i, mt := range m.Mounts {
		mounts[i] = store.MountDecl{AppID: m.Id, Version: req.Msg.Version, Name: mt.Name, Path: mt.Path, Seed: mt.Seed, Required: true, Github: mt.GetGithub()}
	}

	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.Apps().Upsert(ctx, store.App{
			ID: m.Id, DisplayName: m.Title, Summary: m.Description, Tags: strings.Join(m.Tags, ","),
			Visibility: "public", Listed: true, CreatorID: creator, CreatedAt: now,
		}); err != nil {
			return err
		}
		return tx.Apps().UpsertVersion(ctx, store.AppVersion{
			AppID: m.Id, Version: req.Msg.Version, Ref: req.Msg.Ref,
			Tier: store.TierUnverified, Manifest: string(blob), CreatedAt: now,
		}, mounts)
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.RegisterAppVersionResponse{
		AppId: m.Id, Version: req.Msg.Version, Tier: cpv1.TrustTier_TRUST_TIER_UNVERIFIED,
	}), nil
}
