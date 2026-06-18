package cp

// github_mint_resolve_test.go: hermetic tests for T3 — CP auto-resolve of the spawn-owner's
// gh:<owner> mint link-ref, plus the seed+adopt ordering that makes authorizeGitHubMint pass
// BEFORE the hosting node sends its ACTIVE ack (i.e., inside the blocking Provision window).
//
// Test cases:
//   - TestCreateSpawnGitHubSlotSeedsLinkAndPreBindsNodeBeforeProvision (white-box): after a full
//     intent-threaded create reaches ACTIVE, asserts (a) the githubLinkIndex has the spawn indexed,
//     (b) the live container was pre-bound to the target node BEFORE Provision, and (c) the
//     StartSpawn wire message carries GithubMintRef and credential_secret_id both set to gh:<owner>.
//   - TestAuthorizeGitHubMintDeniedBeforeSeed (negative): authorizeGitHubMint returns
//     PermissionDenied for an unseeded spawn.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/authsvc"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

// TestCreateSpawnGitHubSlotSeedsLinkAndPreBindsNodeBeforeProvision drives a full
// intent-threaded create of a github-slot spawn to ACTIVE and asserts:
//  1. The githubLinkIndex has the spawn indexed for "gh:alice" BEFORE the node acks ACTIVE
//     (white-box: we intercept inside the ACK goroutine before calling OnStatus).
//  2. The live container's node_id is the target node (pre-bound by Adopt in prepareGitHubMintProvision).
//  3. authorizeGitHubMint succeeds for the hosting node after the spawn is ACTIVE.
//  4. The StartSpawn wire message carries GithubMintRef.SecretId == "gh:alice" and
//     CredentialSecretId == "gh:alice".
func TestCreateSpawnGitHubSlotSeedsLinkAndPreBindsNodeBeforeProvision(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetIntentEnabled(true)
	s.pendingIntents.ttl = 5 * time.Second

	// Seed a github-slot app.
	seedCreateSpawnGitHubSlotApp(t, s, "gh-slot-resolve", "repo")

	sender := &capSender{}
	s.reg.Add(&registry.Node{ID: "n-intent", Sender: sender, Max: 10, Free: 10})

	// The ACK goroutine intercepts each StartSpawn and, BEFORE acking ACTIVE, verifies that:
	// (a) the link index is seeded (the mint auth call would pass during the Provision window),
	// (b) the live container is pre-bound to n-intent.
	type mintCheckResult struct {
		linkIndexed     bool
		containerNodeID string
		mountRef        string
		mountCred       string
	}
	checkCh := make(chan mintCheckResult, 1)

	stopACK := make(chan struct{})
	go func() {
		seen := 0
		for {
			select {
			case <-stopACK:
				return
			case <-time.After(2 * time.Millisecond):
			}
			starts := sender.starts()
			for len(starts) > seen {
				st := starts[seen]
				seen++
				spawnID := st.GetSpawnId()

				// Capture state BEFORE acking ACTIVE — this is inside the Provision window.
				indexed := s.githubLinks.has("gh:alice", spawnID)
				var containerNodeID string
				if c, ok, _ := s.st.Spawns().LiveContainer(context.Background(), spawnID); ok {
					containerNodeID = c.NodeID
				}
				// Also capture the GithubMintRef from the wire message.
				var mountRef, mountCred string
				for _, mb := range st.GetMounts() {
					if mb.GetName() == "repo" {
						if mb.GetGithubMintRef() != nil {
							mountRef = mb.GetGithubMintRef().GetSecretId()
						}
						mountCred = mb.GetCredentialSecretId()
					}
				}

				select {
				case checkCh <- mintCheckResult{
					linkIndexed:     indexed,
					containerNodeID: containerNodeID,
					mountRef:        mountRef,
					mountCred:       mountCred,
				}:
				default:
				}

				// Now ack ACTIVE.
				s.sched.OnStatus(spawnID, nodev1.SpawnPhase_ACTIVE, "")
			}
		}
	}()
	defer close(stopACK)

	owner := "alice"
	ctx := auth.WithOwner(context.Background(), owner)
	sessionKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	var spawnID string

	// CreateSpawn returns before provision completes; capture the id first.
	var resp *connect.Response[cpv1.CreateSpawnResponse]
	resp, err = s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "gh-slot-resolve",
		Model: "m",
		Mounts: []*cpv1.MountBinding{{
			Name:            "repo",
			BackendUri:      "github:owner/repo",
			CreateIfMissing: true,
			RepositoryId:    "R123",
		}},
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	spawnID = resp.Msg.SpawnId

	// Start the intent-signing goroutine (mirrors the client side).
	goSubmitIntent(context.Background(), s, spawnID, owner, sessionKey, errCh)

	if submitErr := <-errCh; submitErr != nil {
		t.Fatalf("SubmitIntent: %v", submitErr)
	}

	// Wait for the spawn to reach ACTIVE.
	waitIntentSpawnStatus(t, s, spawnID, store.Active)

	// Read the interception result.
	var result mintCheckResult
	select {
	case result = <-checkCh:
	case <-time.After(5 * time.Second):
		t.Fatal("ACK goroutine never fired the check")
	}

	// (a) Link index was seeded BEFORE the ACTIVE ack.
	if !result.linkIndexed {
		t.Error("githubLinkIndex was NOT seeded before the ACTIVE ack (Provision window); seed ordering broken")
	}
	// (b) Live container was pre-bound to n-intent before ACTIVE.
	if result.containerNodeID != "n-intent" {
		t.Errorf("live container node_id = %q before ACTIVE ack, want %q (Adopt pre-bind broken)", result.containerNodeID, "n-intent")
	}
	// (c) Wire message carries GithubMintRef.
	if result.mountRef != "gh:alice" {
		t.Errorf("StartSpawn GithubMintRef.SecretId = %q, want %q", result.mountRef, "gh:alice")
	}
	if result.mountCred != "gh:alice" {
		t.Errorf("StartSpawn CredentialSecretId = %q, want %q for intent correspondence", result.mountCred, "gh:alice")
	}

	// (d) authorizeGitHubMint succeeds post-ACTIVE (belt-and-suspenders: also verifies liveNode).
	authErr := s.authorizeGitHubMint(context.Background(), authsvc.GitHubMintAuthorization{
		NodeID: "n-intent", SpawnID: spawnID, Generation: 1, SecretID: "gh:alice",
	})
	if authErr != nil {
		t.Errorf("authorizeGitHubMint post-ACTIVE: %v", authErr)
	}

	// (e) Persisted mounts carry the gh:alice credential (needed for resume/recreate).
	mounts, err := s.st.Spawns().GetMounts(context.Background(), spawnID)
	if err != nil {
		t.Fatalf("GetMounts: %v", err)
	}
	var repoMount store.Mount
	for _, m := range mounts {
		if m.Name == "repo" {
			repoMount = m
			break
		}
	}
	if repoMount.CredentialSecretID != "gh:alice" {
		t.Errorf("persisted mount CredentialSecretID = %q, want %q", repoMount.CredentialSecretID, "gh:alice")
	}
}

// TestAuthorizeGitHubMintDeniedBeforeSeed verifies that authorizeGitHubMint returns
// PermissionDenied for a spawn whose secret is not in the index (negative baseline).
func TestAuthorizeGitHubMintDeniedBeforeSeed(t *testing.T) {
	s, _, _ := newTestServer(t)
	createActiveSpawn(t, s, "alice", "sp-unseeded", "node-1")
	// Do NOT seed the index.

	err := s.authorizeGitHubMint(context.Background(), authsvc.GitHubMintAuthorization{
		NodeID: "node-1", SpawnID: "sp-unseeded", Generation: 1, SecretID: "gh:alice",
	})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("authorizeGitHubMint for unseeded spawn code=%v err=%v; want PermissionDenied", connect.CodeOf(err), err)
	}
}

// TestPrepareGitHubMintProvisionNoopWhenNoGitHubMintMount verifies that
// prepareGitHubMintProvision is a no-op when the spawn has no gh: mint mount.
func TestPrepareGitHubMintProvisionNoopWhenNoGitHubMintMount(t *testing.T) {
	s, _, _ := newTestServer(t)
	mounts := []store.Mount{{Name: "cache", BackendURI: "scratch"}}
	if err := s.prepareGitHubMintProvision(context.Background(), "any-spawn", 1, "n1", mounts); err != nil {
		t.Fatalf("prepareGitHubMintProvision with no github mount: %v", err)
	}
	// Index should remain empty.
	if s.githubLinks.has("gh:alice", "any-spawn") {
		t.Fatal("index should not be seeded when no gh: mount present")
	}
}

