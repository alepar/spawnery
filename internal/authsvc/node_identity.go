package authsvc

import (
	"context"

	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

func nodeIDFromContext(ctx context.Context) (string, bool) {
	if principal, ok := mtls.PrincipalFromContext(ctx); ok && principal.Kind == pki.KindNode && principal.NodeID != "" {
		return principal.NodeID, true
	}
	return "", false
}
