package cp

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

// ListAgentImages returns the registered agent images and the binaries each ships. The web client
// expands binaries into selectable runnables via the shared agentcaps registry.
func (s *Server) ListAgentImages(ctx context.Context, _ *connect.Request[cpv1.ListAgentImagesRequest]) (*connect.Response[cpv1.ListAgentImagesResponse], error) {
	if _, ok := auth.OwnerFromContext(ctx); !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	images, err := s.st.AgentImages().List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.AgentImageInfo, 0, len(images))
	for _, img := range images {
		bins, err := s.st.AgentImages().Binaries(ctx, img.Image)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		out = append(out, &cpv1.AgentImageInfo{Image: img.Image, CreatedAt: img.CreatedAt, Binaries: bins})
	}
	return connect.NewResponse(&cpv1.ListAgentImagesResponse{Images: out}), nil
}
