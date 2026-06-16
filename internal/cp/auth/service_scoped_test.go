package auth_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/internal/cp/auth"
)

type serviceScopedHandler struct {
	cpv1connect.UnimplementedSpawnServiceHandler
	called bool
}

func (h *serviceScopedHandler) AuthorizeGitHubMint(context.Context, *connect.Request[cpv1.AuthorizeGitHubMintRequest]) (*connect.Response[cpv1.AuthorizeGitHubMintResponse], error) {
	h.called = true
	return connect.NewResponse(&cpv1.AuthorizeGitHubMintResponse{}), nil
}

func (h *serviceScopedHandler) ListSpawns(context.Context, *connect.Request[cpv1.ListSpawnsRequest]) (*connect.Response[cpv1.ListSpawnsResponse], error) {
	h.called = true
	return connect.NewResponse(&cpv1.ListSpawnsResponse{}), nil
}

func TestServiceScopedInterceptorAllowsOnlyConfiguredProcedures(t *testing.T) {
	v := auth.NewVerifier(auth.VerifierConfig{DevMode: false, Now: time.Now})
	h := &serviceScopedHandler{}
	_, handler := cpv1connect.NewSpawnServiceHandler(h, connect.WithInterceptors(auth.NewServiceScopedInterceptor(
		v,
		auth.ServiceSecretHeader,
		"as-secret",
		cpv1connect.SpawnServiceAuthorizeGitHubMintProcedure,
		cpv1connect.SpawnServiceGetGitHubLinkTargetsProcedure,
		cpv1connect.SpawnServiceFanoutGitHubSealedAccessTokenProcedure,
	)))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	client := cpv1connect.NewSpawnServiceClient(ts.Client(), ts.URL, connect.WithInterceptors(testHeader{
		name:  auth.ServiceSecretHeader,
		value: "as-secret",
	}))
	if _, err := client.AuthorizeGitHubMint(context.Background(), connect.NewRequest(&cpv1.AuthorizeGitHubMintRequest{})); err != nil {
		t.Fatalf("AuthorizeGitHubMint with service secret: %v", err)
	}
	if !h.called {
		t.Fatal("allowed github coordination RPC did not reach handler")
	}
	h.called = false

	if _, err := client.ListSpawns(context.Background(), connect.NewRequest(&cpv1.ListSpawnsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("ListSpawns with service secret code=%v err=%v, want Unauthenticated", connect.CodeOf(err), err)
	}
	if h.called {
		t.Fatal("non-github RPC reached handler with service secret")
	}
}

type testHeader struct {
	name  string
	value string
}

func (h testHeader) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set(h.name, h.value)
		return next(ctx, req)
	}
}

func (h testHeader) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (h testHeader) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
