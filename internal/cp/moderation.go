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

// SetAppListing takes down (listed=false) or relists (listed=true) an app. Creator-only.
func (s *Server) SetAppListing(ctx context.Context, req *connect.Request[cpv1.SetAppListingRequest]) (*connect.Response[cpv1.SetAppListingResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	creator, err := s.st.Apps().Creator(ctx, req.Msg.AppId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if creator != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.AppId))
	}
	if err := s.st.Apps().SetListed(ctx, req.Msg.AppId, req.Msg.Listed); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.SetAppListingResponse{}), nil
}
