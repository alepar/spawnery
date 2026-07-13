package cp

// github_credential_status_test.go: the GitHub credential CONDITION (sp-2tx8.9.2, spec §4.1).
// Two things are load-bearing here and both are tested:
//   1. a reported status is persisted and surfaced on ListSpawns;
//   2. an UNREPORTED (UNSPECIFIED) status does NOT clobber a previously reported one — otherwise
//      every routine ACTIVE/STARTING report would silently clear a RELINK_REQUIRED.
// The spawn stays Active throughout: this is a condition, never a phase.

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// summaryFor returns the ListSpawns summary for spawnID as owner, failing if absent.
func summaryFor(t *testing.T, s *Server, owner, spawnID string) *cpv1.SpawnSummary {
	t.Helper()
	ctx := auth.WithOwner(context.Background(), owner)
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatalf("ListSpawns: %v", err)
	}
	for _, sm := range resp.Msg.Spawns {
		if sm.SpawnId == spawnID {
			return sm
		}
	}
	t.Fatalf("%s missing from ListSpawns", spawnID)
	return nil
}

// storedCredStatus reads the persisted condition straight from the store.
func storedCredStatus(t *testing.T, s *Server, spawnID string) store.GitHubCredentialStatus {
	t.Helper()
	sp, err := s.st.Spawns().Get(context.Background(), spawnID)
	if err != nil {
		t.Fatalf("Get %s: %v", spawnID, err)
	}
	return sp.GitHubCredentialStatus
}

// F1: a reported RELINK_REQUIRED is persisted and surfaced on ListSpawns, and the spawn does NOT
// leave Active — it is a condition, not a phase (§4.1).
//
// Unlike F2-F4 (which only need the persist path and use makeSpawn's bare store row), this test
// asserts the actual lifecycle Status, so it must drive a REAL fresh-create through the node
// channel (CreateSpawn -> scheduler.Provision -> node ACTIVE report -> store.SetActive) rather than
// makeSpawn's direct row insert: makeSpawn's spawn has no live container/generation, so a bare
// feedStatusMsg(ACTIVE) never reaches store.Active (OnStatus only unblocks a live Provision waiter;
// see start_progress_test.go for the same CreateSpawn+node-channel pattern).
func TestGitHubCredentialStatusReportedAndSurfaced(t *testing.T) {
	s, reg, _ := newTestServer(t)

	sender := &capSender{}
	in := make(chan *nodev1.NodeMessage, 8)
	go s.runNode(context.Background(), sender, recvFromChan(in))
	feedRegister(in, "n1", "")
	waitNodeClass(t, reg, "n1", "cloud")

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := resp.Msg.SpawnId

	// Wait for the node to actually receive its StartSpawn: only then has scheduler.Provision
	// registered the pending waiter that OnStatus delivers to (see start_progress_test.go for the
	// same wait-for-StartSpawn-before-feeding-status pattern) — feeding the status any earlier races
	// the registration and the report is silently dropped.
	deadline := time.Now().Add(2 * time.Second)
	for sender.firstStart() == nil {
		if time.Now().After(deadline) {
			t.Fatal("no StartSpawn was sent")
		}
		time.Sleep(time.Millisecond)
	}

	// The very report that flips the phase to ACTIVE also carries the credential condition —
	// exactly the shape a real node sends (spec §4.1: one message, two independent facts).
	feedStatusMsg(in, &nodev1.SpawnStatus{
		SpawnId:                id,
		Phase:                  nodev1.SpawnPhase_ACTIVE,
		GithubCredentialStatus: nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_RELINK_REQUIRED,
	})
	waitActive(t, s, id)

	waitCondition(t, "relink_required persisted", func() bool {
		return storedCredStatus(t, s, id) == store.GitHubCredRelinkRequired
	})

	sm := summaryFor(t, s, "alice", id)
	if sm.GithubCredentialStatus != cpv1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_RELINK_REQUIRED {
		t.Fatalf("SpawnSummary.github_credential_status = %v, want RELINK_REQUIRED", sm.GithubCredentialStatus)
	}
	// A condition, not a phase: the ACTIVE report must still have made the spawn Active.
	if sm.Status != cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE {
		t.Fatalf("spawn status = %v, want ACTIVE (the condition must not touch the phase)", sm.Status)
	}
}

// F2 (the important one): a status message that does NOT report a credential status must NOT clear
// a previously reported one. Every plain STARTING/ACTIVE/milestone report leaves the field at 0.
func TestGitHubCredentialStatusUnspecifiedDoesNotClobber(t *testing.T) {
	s, reg, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	in := make(chan *nodev1.NodeMessage, 8)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	feedRegister(in, "n1", "")
	waitNodeClass(t, reg, "n1", "cloud")

	feedStatusMsg(in, &nodev1.SpawnStatus{
		SpawnId:                "sp1",
		Phase:                  nodev1.SpawnPhase_ACTIVE,
		GithubCredentialStatus: nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_STALE,
	})
	waitCondition(t, "stale persisted", func() bool {
		return storedCredStatus(t, s, "sp1") == store.GitHubCredStale
	})

	// A routine report with the field unset (exactly what attacher.status/statusActive send).
	feedStatusMsg(in, &nodev1.SpawnStatus{SpawnId: "sp1", Phase: nodev1.SpawnPhase_ACTIVE})
	time.Sleep(20 * time.Millisecond) // let the receive loop drain it

	if got := storedCredStatus(t, s, "sp1"); got != store.GitHubCredStale {
		t.Fatalf("unreported (UNSPECIFIED) status clobbered the condition: got %q, want %q", got, store.GitHubCredStale)
	}
}

// F3: an explicit OK clears a prior STALE (the self-healing path: the node's next push succeeded).
func TestGitHubCredentialStatusExplicitOKClears(t *testing.T) {
	s, reg, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	in := make(chan *nodev1.NodeMessage, 8)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	feedRegister(in, "n1", "")
	waitNodeClass(t, reg, "n1", "cloud")

	feedStatusMsg(in, &nodev1.SpawnStatus{
		SpawnId:                "sp1",
		Phase:                  nodev1.SpawnPhase_ACTIVE,
		GithubCredentialStatus: nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_STALE,
	})
	waitCondition(t, "stale persisted", func() bool {
		return storedCredStatus(t, s, "sp1") == store.GitHubCredStale
	})

	feedStatusMsg(in, &nodev1.SpawnStatus{
		SpawnId:                "sp1",
		Phase:                  nodev1.SpawnPhase_ACTIVE,
		GithubCredentialStatus: nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_OK,
	})
	waitCondition(t, "explicit OK clears stale", func() bool {
		return storedCredStatus(t, s, "sp1") == store.GitHubCredOK
	})

	sm := summaryFor(t, s, "alice", "sp1")
	if sm.GithubCredentialStatus != cpv1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_OK {
		t.Fatalf("SpawnSummary.github_credential_status = %v, want OK", sm.GithubCredentialStatus)
	}
}

// F4: a spawn that never reports carries UNSPECIFIED on the view (no GitHub mount → no condition).
func TestGitHubCredentialStatusUnreportedIsUnspecified(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	sm := summaryFor(t, s, "alice", "sp1")
	if sm.GithubCredentialStatus != cpv1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_UNSPECIFIED {
		t.Fatalf("never-reported spawn: got %v, want UNSPECIFIED", sm.GithubCredentialStatus)
	}
}
