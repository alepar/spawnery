package node

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/githubcred"
	"spawnery/internal/intent"
	"spawnery/internal/runtime"
	"spawnery/internal/runtime/fakepod"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage"
)

type blockingInitialMintClient struct {
	entered chan struct{}
	calls   atomic.Int32
}

func (c *blockingInitialMintClient) MintGitHubAccessToken(ctx context.Context, _ *connect.Request[authv1.MintGitHubAccessTokenRequest]) (*connect.Response[authv1.MintGitHubAccessTokenResponse], error) {
	if c.calls.Add(1) == 1 {
		close(c.entered)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type countingStartBackend struct {
	runtime.PodBackend
	starts atomic.Int32
}

func (b *countingStartBackend) StartPod(ctx context.Context, spec runtime.PodSpec) (*runtime.PodHandle, error) {
	b.starts.Add(1)
	return b.PodBackend.StartPod(ctx, spec)
}

func TestConcurrentConflictingStartCannotMutateWinnerTransientState(t *testing.T) {
	now := time.Unix(1_770_000_000, 0)
	signer, verifier := genASKey(t)
	backend := &countingStartBackend{PodBackend: fakeBackend(t, fakepod.WithAttachScript(scriptGoose))}
	mgr := spawnlet.NewManagerWithBackend(backend, noopApplier{}, spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	stream := &fakeCPStream{}
	a := newEnforcedAttacher(t, mgr, stream, verifier, now)
	mint := &blockingInitialMintClient{entered: make(chan struct{})}
	a.githubRefresh = newGitHubRefresher(mint)
	app := writeNodeApp(t)
	mounts := []*nodev1.MountBinding{{
		Name: "main", BackendUri: "github:octo/demo", RepositoryId: "42",
		GithubMintRef: &nodev1.GitHubMintRef{SecretId: "gh:octo"},
	}}
	start := func(account, jti string) *nodev1.StartSpawn {
		body := &authv1.IntentBody{
			Jti: jti, IssuedAt: now.Unix(), SpawnId: "sp-conflict", Generation: 1,
			Op: string(intent.OpCreateSpawn), AppRef: app, Model: "m",
			Mounts: []*authv1.MountRef{{Name: "main", BackendUri: "github:octo/demo", RepositoryId: "42"}},
		}
		return &nodev1.StartSpawn{
			SpawnId: "sp-conflict", Generation: 1, AppRef: app, Model: "m", Mounts: mounts,
			AssertedOwner: account, IntentOp: string(intent.OpCreateSpawn),
			Auth: buildIntentEnvelope(t, signer, verifier, genECDSA(t), account, now, body, intent.OpCreateSpawn),
		}
	}

	winnerCtx, cancelWinner := context.WithCancel(context.Background())
	winnerDone := make(chan struct{})
	go func() {
		defer close(winnerDone)
		a.startSpawn(winnerCtx, start("alice", "winner"))
	}()
	<-mint.entered

	if _, err := mgr.RenderGitHubNodeCredential("sp-conflict", "main", githubcred.RenderRequest{
		Owner: "octo", Repo: "demo", AccessToken: "winner-token",
	}); err != nil {
		t.Fatal(err)
	}
	a.startSpawn(context.Background(), start("mallory", "loser"))

	if got := mint.calls.Load(); got != 1 {
		t.Fatalf("mint calls = %d, loser crossed reservation boundary", got)
	}
	if got := backend.starts.Load(); got != 0 {
		t.Fatalf("pod starts = %d before winner mint completed", got)
	}
	credential, err := mgr.TokenForGitHubMount(context.Background(), "sp-conflict", "main", storage.GitHubConfig{})
	if err != nil {
		t.Fatalf("loser deleted winner credential: %v", err)
	}
	if token, err := credential.Token(); err != nil || token != "winner-token" {
		t.Fatalf("winner credential = %q, err=%v", token, err)
	}

	cancelWinner()
	<-winnerDone
}
