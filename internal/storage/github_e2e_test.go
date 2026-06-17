//go:build github_e2e

package storage

// github_e2e tests cover the GitHub backend's create→commit→suspend→resume
// mechanics against a local Gitea instance using a static personal access token.
// This decouples the backend-mechanics lane from the OAuth/AS mint path
// (sp-v40s) so the suite does not hard-block on sp-v40s availability.
//
// Required environment:
//   GITEA_URL   — base URL of the running Gitea instance, e.g. http://localhost:3000
//   GITEA_TOKEN — Gitea personal access token (Settings → Applications)
//   GITEA_OWNER — Gitea username/org that will own the ephemeral test repos
//
// Tests in this file FAIL (never t.Skip) when any required env var is absent,
// naming the missing dep and how to provide it — per project test convention.
//
// Spec: docs/superpowers/specs/2026-06-14-github-credentials-and-storage-unified-design.md
//   §8  backend mechanics (Prepare/Finalize)
//   §16.7 suspend backstop deferred; github: mounts MUST be journaled
//   §12  this lane covers: clone, local commit, journal snapshot/restore, no backstop refs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnery/internal/storage/journal"
)

// requireGiteaEnv reads the three required env vars or t.Fatalf with a clear
// dep-missing message naming the missing var and how to provide it.
func requireGiteaEnv(t *testing.T) (giteaURL, giteaToken, giteaOwner string) {
	t.Helper()
	for _, kv := range []struct {
		key  string
		dst  *string
		hint string
	}{
		{"GITEA_URL", &giteaURL, "set GITEA_URL=http://localhost:3000 and start Gitea"},
		{"GITEA_TOKEN", &giteaToken, "set GITEA_TOKEN to a Gitea personal access token (Settings → Applications)"},
		{"GITEA_OWNER", &giteaOwner, "set GITEA_OWNER to the Gitea username or org"},
	} {
		v := os.Getenv(kv.key)
		if v == "" {
			t.Fatalf("github_e2e: required env var %s is not set; %s", kv.key, kv.hint)
		}
		*kv.dst = v
	}
	return
}

// giteaHost extracts the host (host:port) from a gitea base URL.
func giteaHost(t *testing.T, giteaURL string) string {
	t.Helper()
	u, err := url.Parse(giteaURL)
	if err != nil {
		t.Fatalf("github_e2e: invalid GITEA_URL %q: %v", giteaURL, err)
	}
	if u.Host == "" {
		t.Fatalf("github_e2e: GITEA_URL %q has no host", giteaURL)
	}
	return u.Host
}

// writeCredHelper writes a git credential helper script that outputs a static
// token for all hosts. Returns the absolute path to the executable script.
// The script ignores stdin and always echoes the same credentials, which is
// correct for a single-token test environment.
func writeCredHelper(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git-cred-helper")
	// The token is a gitea PAT — alphanumeric, no shell-special characters.
	content := fmt.Sprintf("#!/bin/sh\nprintf 'username=git\\npassword=%s\\n'\n", token)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write cred helper: %v", err)
	}
	return path
}

// runGit runs a git command in dir, returns combined output. Calls t.Fatalf on
// error.
func runGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
	return out
}

// staticGiteaCreds implements GitHubCredentialProvider with a fixed token and
// pre-written credential helper path. Used in the github_e2e lane only.
type staticGiteaCreds struct {
	token      string
	helperPath string
}

func (c staticGiteaCreds) TokenForGitHubMount(_ context.Context, _, _ string, _ GitHubConfig) (GitHubCredential, error) {
	return GitHubCredential{AccessToken: c.token, CredentialHelperPath: c.helperPath}, nil
}

// giteaAPICall performs an authenticated REST call against gitea's API.
func giteaAPICall(t *testing.T, method, giteaURL, token, path string, body any) ([]byte, int) {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method,
		strings.TrimRight(giteaURL, "/")+"/api/v1"+path,
		bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "token "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gitea API %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBytes := make([]byte, 0, 512)
	buf := bytes.Buffer{}
	_, _ = buf.ReadFrom(resp.Body)
	respBytes = buf.Bytes()
	return respBytes, resp.StatusCode
}

// deleteGiteaRepo deletes the ephemeral test repo; called in t.Cleanup.
func deleteGiteaRepo(t *testing.T, giteaURL, token, owner, repo string) {
	t.Helper()
	_, status := giteaAPICall(t, http.MethodDelete, giteaURL, token, "/repos/"+owner+"/"+repo, nil)
	if status != http.StatusNoContent && status != http.StatusNotFound {
		t.Logf("warning: delete gitea repo %s/%s status %d (cleanup best-effort)", owner, repo, status)
	}
}

// assertNoBackstopRefs verifies that no refs/spawnery/backstop/* refs exist on the
// remote. Per spec §16.7, the suspend backstop is deferred to sp-u53.8; github: mounts
// in MVP rely on the Kopia journal for cross-node durability of committed work.
func assertNoBackstopRefs(t *testing.T, cloneURL, credHelperPath string) {
	t.Helper()
	// git ls-remote --refs lists all refs under refs/ (not HEAD). An empty remote has no
	// output and exits 0. refs/spawnery/backstop/* would appear if backstop were pushed.
	cmd := exec.Command("git",
		"-c", "credential.helper=",
		"-c", "credential.helper="+credHelperPath,
		"-c", "credential.useHttpPath=true",
		"ls-remote", "--refs", cloneURL,
	)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")
	out, _ := cmd.CombinedOutput() // exit non-zero on empty remote is OK; we inspect output
	if bytes.Contains(out, []byte("refs/spawnery/backstop")) {
		t.Errorf("backstop refs found on remote — must not exist in MVP (suspend backstop "+
			"deferred to sp-u53.8; committed .git is durable via journal §16.7):\n%s", out)
	}
}

// newGitHubE2EJournalManager creates a filesystem-backed, node-local-custody journal
// manager for use in github_e2e tests. It mirrors newTestManager in manager_test.go
// but is accessible from package storage (journal is an internal sub-package, not
// package journal, so the test helper cannot be shared).
func newGitHubE2EJournalManager(t *testing.T) *journal.Manager {
	t.Helper()
	root := t.TempDir()
	keyfile := filepath.Join(root, "node.key")
	if err := journal.GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatal(err)
	}
	custody, err := journal.NewNodeLocalCustody(keyfile, filepath.Join(root, "seals"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := journal.NewManager(journal.Config{
		RepoRoot: filepath.Join(root, "repos"),
		Backend:  &journal.FilesystemBackend{Root: filepath.Join(root, "blobs")},
		Custody:  custody,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestGitHubE2ECreateCommitSuspendResume exercises the full backend-mechanics
// flow against a local Gitea instance using a static personal access token:
//
//  1. Prepare (create repo + clone) — uses the GitHub backend against Gitea's
//     GitHub-compatible API; AllowInsecureHost=true permits HTTP.
//  2. Agent commit (local, NOT pushed) — simulates an agent writing and committing
//     work without pushing, the normal in-spawn Git workflow.
//  3. Suspend — FinalSnapshot captures the entire working tree including .git
//     (committed-but-unpushed work is already durable in the journal).
//  4. Finalize — cleans up the clone dir.
//  5. Resume — SetRestorePending(true) + Prepare provides an empty dir for the
//     journal to restore into (no re-clone, journal is authoritative; §16.7).
//  6. Restore — journal restores the working tree, including the unpushed commit.
//
// Assertions:
//   - The restored working tree contains the agent's file and exact commit hash.
//   - No refs/spawnery/backstop/* refs exist on the remote (backstop deferred
//     to sp-u53.8; cross-node durability of committed .git is via journal §16.7).
func TestGitHubE2ECreateCommitSuspendResume(t *testing.T) {
	ctx := context.Background()

	giteaURL, giteaToken, giteaOwner := requireGiteaEnv(t)
	host := giteaHost(t, giteaURL)

	// Each test run uses an isolated repo so parallel/repeated runs do not clash.
	repoName := fmt.Sprintf("spawnery-e2e-%d", time.Now().UnixNano())
	cloneURL := fmt.Sprintf("%s/%s/%s.git", strings.TrimRight(giteaURL, "/"), giteaOwner, repoName)
	t.Cleanup(func() { deleteGiteaRepo(t, giteaURL, giteaToken, giteaOwner, repoName) })

	credHelper := writeCredHelper(t, giteaToken)

	jm := newGitHubE2EJournalManager(t)

	// --- Step 1: Prepare (create repo + clone) ----------------------------

	const (
		spawnID   = "spawn-github-e2e"
		mountName = "src"
	)
	var gen uint64 = 1

	storageRoot := t.TempDir()
	seedDir := t.TempDir() // empty: no seed files; repo stays empty until agent commits

	backend := &GitHub{
		Root: storageRoot,
		Config: GitHubConfig{
			Host:              host,
			Owner:             giteaOwner,
			Repo:              repoName,
			CreateIfMissing:   true,
			AllowInsecureHost: true, // local gitea; HTTP is fine in the test lane
		},
		Credentials: staticGiteaCreds{token: giteaToken, helperPath: credHelper},
		Repos: &defaultGitHubRepoService{
			Client:  http.DefaultClient,
			BaseURL: strings.TrimRight(giteaURL, "/") + "/api/v1",
		},
		// Git: nil → execGitRunner (real git)
	}

	hostDir, err := backend.Prepare(ctx, spawnID, mountName, seedDir, -1)
	if err != nil {
		t.Fatalf("Prepare (create+clone): %v", err)
	}
	if _, err := os.Stat(filepath.Join(hostDir, ".git")); err != nil {
		t.Fatalf("cloned dir missing .git: %v", err)
	}

	// --- Step 2: Agent commit (local, NOT pushed) --------------------------
	// Simulates the agent writing and committing work. The commit is intentionally
	// not pushed; the journal must capture it for cross-node resume.

	agentFile := filepath.Join(hostDir, "agent-work.txt")
	if err := os.WriteFile(agentFile, []byte("agent work content"), 0o644); err != nil {
		t.Fatalf("write agent file: %v", err)
	}
	runGit(t, hostDir, "add", "agent-work.txt")
	runGit(t, hostDir,
		"-c", "user.name=Agent",
		"-c", "user.email=agent@example.invalid",
		"commit", "-m", "Agent commit (unpushed local work)",
	)

	agentCommitHash := strings.TrimSpace(string(runGit(t, hostDir, "rev-parse", "HEAD")))
	t.Logf("agent commit hash: %s", agentCommitHash)

	// Sanity: the remote does NOT have the commit yet (it was not pushed).
	lsOut, _ := func() ([]byte, error) {
		cmd := exec.Command("git",
			"-c", "credential.helper=",
			"-c", "credential.helper="+credHelper,
			"-c", "credential.useHttpPath=true",
			"ls-remote", "--refs", cloneURL,
		)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")
		return cmd.CombinedOutput()
	}()
	if bytes.Contains(lsOut, []byte(agentCommitHash)) {
		t.Fatalf("agent commit %s already on remote before journal suspend — test invariant violated", agentCommitHash)
	}

	// --- Step 3: Suspend — journal FinalSnapshot captures .git -----------
	// The Kopia journal snapshots the full working tree including .git, so
	// committed-but-unpushed work is captured (spec §10 / §16.7 rationale).

	jMount := journal.Mount{Name: mountName, HostDir: hostDir, Class: journal.NodeLocal}
	pins, err := jm.FinalSnapshot(ctx, spawnID, gen, []journal.Mount{jMount})
	if err != nil {
		t.Fatalf("FinalSnapshot: %v", err)
	}
	manifestID := pins[mountName]
	if manifestID == "" {
		t.Fatal("FinalSnapshot returned empty manifest id — mount was not journaled")
	}
	t.Logf("journal manifest id: %s", manifestID)

	// --- Step 4: Finalize (clean up the clone dir) -----------------------

	if err := backend.Finalize(ctx, hostDir); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if _, err := os.Stat(hostDir); !os.IsNotExist(err) {
		t.Fatalf("host dir should not exist after Finalize, stat err=%v", err)
	}

	// --- Step 5: Resume — SetRestorePending + Prepare ---------------------
	// On resume, the journal is authoritative (spec §16.7). Prepare with
	// restorePending=true skips all network operations and provides an empty
	// host dir for the journal to restore into.

	// Release gen-1 journal state so Restore can reopen.
	if err := jm.Close(ctx, spawnID); err != nil {
		t.Fatalf("journal Close: %v", err)
	}

	resumeBackend := &GitHub{
		Root: storageRoot,
		Config: GitHubConfig{
			Host:              host,
			Owner:             giteaOwner,
			Repo:              repoName,
			AllowInsecureHost: true,
		},
		Credentials: staticGiteaCreds{token: giteaToken, helperPath: credHelper},
		Repos: &defaultGitHubRepoService{
			Client:  http.DefaultClient,
			BaseURL: strings.TrimRight(giteaURL, "/") + "/api/v1",
		},
	}
	resumeBackend.SetRestorePending(true)

	resumeDir, err := resumeBackend.Prepare(ctx, spawnID, mountName, seedDir, -1)
	if err != nil {
		t.Fatalf("Prepare (resume/restorePending): %v", err)
	}
	entries, err := os.ReadDir(resumeDir)
	if err != nil {
		t.Fatalf("read resumeDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Prepare with restorePending=true must return empty dir (journal fills it); got %v", entries)
	}

	// --- Step 6: Restore from journal -------------------------------------

	if err := jm.Restore(ctx, spawnID, mountName, manifestID, resumeDir); err != nil {
		t.Fatalf("journal Restore: %v", err)
	}

	// Assert: agent file is present with correct content.
	got, err := os.ReadFile(filepath.Join(resumeDir, "agent-work.txt"))
	if err != nil {
		t.Fatalf("agent-work.txt missing after journal restore: %v", err)
	}
	if string(got) != "agent work content" {
		t.Fatalf("agent-work.txt = %q, want %q", got, "agent work content")
	}

	// Assert: restored .git has the exact commit hash (unpushed commit is durable
	// via journal, not via backstop or a re-clone).
	restoredHash := strings.TrimSpace(string(runGit(t, resumeDir, "rev-parse", "HEAD")))
	if restoredHash != agentCommitHash {
		t.Fatalf("restored HEAD = %q, want %q (journal must capture the unpushed commit)", restoredHash, agentCommitHash)
	}

	// --- BACKSTOP ASSERTION (spec §16.7) ----------------------------------
	// The suspend backstop is deferred to sp-u53.8. In MVP, no refs/spawnery/backstop/*
	// refs must exist on the remote: durability is via the Kopia journal only.
	assertNoBackstopRefs(t, cloneURL, credHelper)
}

// TestGitHubE2EJournaledMountRequirement verifies the DurabilityClass invariant
// mandated by spec §16.7: github: mounts MUST use a journaled durability class.
// The suspend backstop is deferred to sp-u53.8; until then, committed .git is
// cross-node durable ONLY via the Kopia journal — a non-journaled github: mount
// has no durability guarantee.
//
// This test asserts the journal.DurabilityClass semantics that sp-u53.1.1 (spawnlet
// bind validation) must enforce when wiring per-mount backends.
func TestGitHubE2EJournaledMountRequirement(t *testing.T) {
	// Ephemeral is NOT journaled — a github: mount with this class has no durability.
	if journal.Ephemeral.Journaled() {
		t.Error("journal.Ephemeral.Journaled() = true, want false: Ephemeral mounts must not be journaled")
	}
	// NodeLocal IS journaled — the expected class for MVP github: mounts.
	if !journal.NodeLocal.Journaled() {
		t.Error("journal.NodeLocal.Journaled() = false, want true: NodeLocal must be journaled")
	}
	// OwnerSealed IS journaled — required for cross-node github: mounts.
	if !journal.OwnerSealed.Journaled() {
		t.Error("journal.OwnerSealed.Journaled() = false, want true: OwnerSealed must be journaled")
	}
	// sp-u53.1.1 must call class.Journaled() in spawnlet bind validation and reject
	// github: mounts where !class.Journaled(). This test documents the contract.
}
