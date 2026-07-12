package authsvc

import (
	"context"

	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

type nodeIdentityContextKey struct{}

func nodeIDFromContext(ctx context.Context) (string, bool) {
	if principal, ok := mtls.PrincipalFromContext(ctx); ok && principal.Kind == pki.KindNode && principal.NodeID != "" {
		return principal.NodeID, true
	}
	id, ok := ctx.Value(nodeIdentityContextKey{}).(pki.Principal)
	return id.NodeID, ok && id.NodeID != ""
}

func withNodeIdentity(ctx context.Context, id pki.Principal) context.Context {
	return context.WithValue(ctx, nodeIdentityContextKey{}, id)
}
