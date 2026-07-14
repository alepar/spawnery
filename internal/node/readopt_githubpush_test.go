package node

import (
	"context"
	"net/http"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime/fakepod"
)

// Spec §3.1 lists THREE delivery points for the pushed GitHub credentials: sidecar-ready, rotation, and
// RE-ADOPT. The first two were pinned; re-adopt was not.
//
// The gap was real, not theoretical: deleting the `a.ghControl.PushAsync(ctx, sp.ID)` call from
// readopt.go left BOTH internal/node and internal/spawnlet green. Nothing noticed. A node restart would
// then silently lose the credential re-push AND the rejection long-poll (PushAsync's success path is what
// re-establishes the watch), and CI would be perfectly happy — which is exactly the class of bug this epic
// exists to eliminate.
//
// The existing TestPushAsyncReEstablishesTheWatchOnReAdopt is misleadingly named: it calls PushAsync
// DIRECTLY and never drives adoptPod, so it cannot catch a missing call site.
//
// This test drives the real adopt path and asserts the sidecar actually RECEIVED the push.
func TestAdoptPodPushesCredentialsToTheSidecar(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	app := writeNodeApp(t)

	first := restartedNode(t, be, dataRoot, &fakeCPStream{})
	if _, err := first.mgr.Create(ctx, "sp1", app, "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The "restarted" node: a fresh process over the same still-running pod.
	a := restartedNode(t, be, dataRoot, &fakeCPStream{})
	a.githubRefresh = newGitHubRefresher(freshFakeMintClient("ghs_readopt", time.Now().Add(2*time.Hour).Unix()))
	// Wire the control server the way attach.go:219-220 wires it in production (an HTTP client + a
	// spawn->sidecar lookup). The restartedNode helper does NOT do this, which is precisely why no
	// adopt-path test could ever observe a push: without .doer, PushAsync bails with "no HTTP client",
	// so deleting the call site changed nothing observable. Tighten the backoff so a failure surfaces
	// as a fast red rather than a timeout.
	sc := newFakeSidecar(t)
	a.ghControl = newGitHubControlServer(a.githubRefresh, caStore{dir: t.TempDir()})
	a.ghControl.doer = &http.Client{Timeout: 2 * time.Second}
	a.ghControl.pushBackoffBase = time.Millisecond
	a.ghControl.pushBackoffMax = 2 * time.Millisecond
	a.ghControl.lookup = func(string) (string, string, bool) { return sc.controlURL(), "tok", true }

	pods, err := a.mgr.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}

	if err := a.adoptPod(ctx, pods[0], &nodev1.AdoptSpawn{Spec: &nodev1.StartSpawn{
		SpawnId: "sp1", AppRef: app, Model: "model", Generation: 1,
		Mounts: []*nodev1.MountBinding{
			{Name: "main", RepositoryId: "owner/repo", GithubMintRef: &nodev1.GitHubMintRef{SecretId: "gh:owner"}},
		},
	}}); err != nil {
		t.Fatalf("adoptPod: %v", err)
	}

	// PushAsync is asynchronous by contract, so poll rather than sleep-and-hope.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := sc.pushes(); len(got) > 0 {
			if got[0].Token != "ghs_readopt" {
				t.Fatalf("re-adopt pushed token %q, want ghs_readopt", got[0].Token)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("adoptPod never pushed credentials to the sidecar — spec §3.1's re-adopt delivery " +
				"point is missing, so a node restart silently loses the credential re-push AND the " +
				"rejection long-poll that PushAsync's success path re-establishes")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
