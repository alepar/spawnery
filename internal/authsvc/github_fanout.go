package authsvc

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
)

// GitHubSignalCP is the CP-side surface the token-free rotation signal adapter needs.
type GitHubSignalCP interface {
	SignalGitHubTokenRotated(context.Context, *connect.Request[cpv1.SignalGitHubTokenRotatedRequest]) (*connect.Response[cpv1.SignalGitHubTokenRotatedResponse], error)
}

type cpGitHubTokenRotatedNotifier struct {
	cp GitHubSignalCP
}

// NewCPGitHubTokenRotatedNotifier returns a GitHubTokenRotatedNotifier that forwards the rotation
// signal to the CP via SignalGitHubTokenRotated. The CP expands to per-spawn CPMessage signals;
// no token ever transits the CP.
func NewCPGitHubTokenRotatedNotifier(cp GitHubSignalCP) GitHubTokenRotatedNotifier {
	return &cpGitHubTokenRotatedNotifier{cp: cp}
}

func (n *cpGitHubTokenRotatedNotifier) SignalGitHubTokenRotated(ctx context.Context, sig GitHubTokenRotatedSignal) error {
	if n.cp == nil {
		return fmt.Errorf("github signal cp client is not configured")
	}
	_, err := n.cp.SignalGitHubTokenRotated(ctx, connect.NewRequest(&cpv1.SignalGitHubTokenRotatedRequest{
		SecretId:           sig.SecretID,
		Version:            sig.Version,
		DeliveryId:         sig.DeliveryID,
		AccessExpiresAtUnix: sig.AccessExpiresAtUnix,
	}))
	return err
}
