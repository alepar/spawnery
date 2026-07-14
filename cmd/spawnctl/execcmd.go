package main

import (
	"context"
	"fmt"
	"io"

	clientpkg "spawnery/internal/client"
)

type authenticatedExecClient interface {
	Exec(context.Context, string, []string, io.Writer, io.Writer) (int, error)
}

func buildAuthenticatedExecClient(cpAddr string, source *cpTokenSource, trust clientpkg.TargetTrust) (authenticatedExecClient, error) {
	sdk := clientpkg.New(cpAddr, source, nil, clientpkg.WithNodeAuthorization(source, trust))
	if err := sdk.PreflightNodeAuthorization(context.Background()); err != nil {
		return nil, fmt.Errorf("node authorization: %w", err)
	}
	return sdk, nil
}

func runExec(ctx context.Context, client authenticatedExecClient, spawn string, argv []string, stdout, stderr io.Writer) (int, error) {
	if client == nil {
		return 1, fmt.Errorf("authenticated exec client is required")
	}
	return client.Exec(ctx, spawn, argv, stdout, stderr)
}
