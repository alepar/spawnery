package authsvc

import (
	"context"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
)

type GitHubMintCP interface {
	AuthorizeGitHubMint(context.Context, *connect.Request[cpv1.AuthorizeGitHubMintRequest]) (*connect.Response[cpv1.AuthorizeGitHubMintResponse], error)
}

type cpGitHubMintAuthorizer struct {
	cp GitHubMintCP
}

func NewCPGitHubMintAuthorizer(cp GitHubMintCP) GitHubMintAuthorizer {
	return cpGitHubMintAuthorizer{cp: cp}
}

func (a cpGitHubMintAuthorizer) AuthorizeGitHubMint(ctx context.Context, req GitHubMintAuthorization) error {
	_, err := a.cp.AuthorizeGitHubMint(ctx, connect.NewRequest(&cpv1.AuthorizeGitHubMintRequest{
		NodeId:       req.NodeID,
		SpawnId:      req.SpawnID,
		Generation:   req.Generation,
		SecretId:     req.SecretID,
		Version:      req.Version,
		DeliveryId:   req.DeliveryID,
		RepositoryId: req.RepositoryID,
	}))
	return err
}
