package authsvc

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
)

type fakeGitHubSignalCP struct {
	got *cpv1.SignalGitHubTokenRotatedRequest
}

func (f *fakeGitHubSignalCP) SignalGitHubTokenRotated(_ context.Context, req *connect.Request[cpv1.SignalGitHubTokenRotatedRequest]) (*connect.Response[cpv1.SignalGitHubTokenRotatedResponse], error) {
	f.got = req.Msg
	return connect.NewResponse(&cpv1.SignalGitHubTokenRotatedResponse{}), nil
}

func TestCPGitHubTokenRotatedNotifierForwardsSignalFields(t *testing.T) {
	cp := &fakeGitHubSignalCP{}
	notifier := NewCPGitHubTokenRotatedNotifier(cp)

	err := notifier.SignalGitHubTokenRotated(context.Background(), GitHubTokenRotatedSignal{
		SecretID:            "gh-main",
		Version:             12,
		DeliveryID:          "github-access-gh-main-v12",
		AccessExpiresAtUnix: 1893420000,
	})
	if err != nil {
		t.Fatalf("signal: %v", err)
	}
	if cp.got == nil {
		t.Fatal("CP SignalGitHubTokenRotated was not called")
	}
	if cp.got.GetSecretId() != "gh-main" || cp.got.GetVersion() != 12 ||
		cp.got.GetDeliveryId() != "github-access-gh-main-v12" || cp.got.GetAccessExpiresAtUnix() != 1893420000 {
		t.Fatalf("signal fields = %+v", cp.got)
	}
	// Containment: the signal carries no token field.
	if fd := cp.got.ProtoReflect().Descriptor().Fields().ByName("access_token"); fd != nil {
		t.Fatalf("SignalGitHubTokenRotatedRequest must not expose an access_token field")
	}
}
