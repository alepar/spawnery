package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testGitHubCreds struct{}

func (testGitHubCreds) TokenForGitHubMount(context.Context, string, string, GitHubConfig) (GitHubCredential, error) {
	return GitHubCredential{AccessToken: "SECRET-GITHUB-TOKEN", CredentialHelperPath: "/node-only/helper"}, nil
}

type testGitHubRepos struct {
	getErr      error
	coverageErr error // forces the post-create coverage GET to fail
	created     bool
	url         string
	id          int64
	getCalls    int
}

func (r *testGitHubRepos) Get(context.Context, GitHubConfig, string) (GitHubRepoInfo, error) {
	r.getCalls++
	if r.created { // post-create coverage GET: the live token now reaches the repo
		if r.coverageErr != nil {
			return GitHubRepoInfo{}, r.coverageErr
		}
		return GitHubRepoInfo{CloneURL: r.url, Empty: true, ID: r.id}, nil
	}
	if r.getErr != nil {
		return GitHubRepoInfo{}, r.getErr
	}
	return GitHubRepoInfo{CloneURL: r.url, Empty: false, ID: r.id}, nil
}

func (r *testGitHubRepos) Create(context.Context, GitHubConfig, string) (GitHubRepoInfo, error) {
	r.created = true
	return GitHubRepoInfo{CloneURL: r.url, Empty: true, ID: r.id}, nil
}

type gitCall struct {
	dir  string
	env  []string
	args []string
}

type testGitRunner struct {
	calls   []gitCall
	failArg string
}

func (r *testGitRunner) RunGit(_ context.Context, dir string, env []string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, gitCall{
		dir:  dir,
		env:  append([]string(nil), env...),
		args: append([]string(nil), args...),
	})
	for _, arg := range args {
		if arg == r.failArg {
			return []byte("injected failure"), errors.New("git failed")
		}
	}
	for _, arg := range args {
		if arg == "clone" {
			hostDir := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(hostDir, ".git"), 0o755); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}
	return nil, nil
}

func TestGitHubPrepareCreatesMissingRepoSeedsAndUsesHardenedGit(t *testing.T) {
	root := t.TempDir()
	seed := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	repos := &testGitHubRepos{getErr: ErrGitHubRepoNotFound, url: "https://github.com/octo/demo.git"}
	runner := &testGitRunner{}
	backend := &GitHub{
		Root:        root,
		Config:      GitHubConfig{Host: "github.com", Owner: "octo", Repo: "demo", CreateIfMissing: true, CredentialSecretID: "gh-main"},
		Credentials: testGitHubCreds{},
		Repos:       repos,
		Git:         runner,
	}

	hostDir, err := backend.Prepare(context.Background(), "sp1", "main", seed, -1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !repos.created {
		t.Fatal("missing repo was not created")
	}
	if _, err := os.Stat(filepath.Join(hostDir, "README.md")); err != nil {
		t.Fatalf("seed not copied into clone: %v", err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("git calls = %d, want clone/add/commit/push: %+v", len(runner.calls), runner.calls)
	}
	if !containsArg(runner.calls[2].args, "user.name=Spawnery") || !containsArg(runner.calls[2].args, "user.email=spawnery@example.invalid") {
		t.Fatalf("seed commit args missing explicit identity config: %+v", runner.calls[2].args)
	}
	joined := strings.Builder{}
	for _, call := range runner.calls {
		joined.WriteString(strings.Join(call.env, "\x00"))
		joined.WriteString("\x00")
		joined.WriteString(strings.Join(call.args, "\x00"))
		joined.WriteString("\x00")
		if !containsArg(call.args, "-c") || !containsArg(call.args, "credential.helper=") || !containsArg(call.args, "credential.helper=/node-only/helper") {
			t.Fatalf("git args missing credential helper reset/helper: %+v", call.args)
		}
		// CVE-2024-53858 / clone2leak hardening (§16.8): all node-side git ops must set
		// credential.protectProtocol=true to refuse credential delivery on protocol downgrade.
		if !containsArg(call.args, "credential.protectProtocol=true") {
			t.Fatalf("git args missing credential.protectProtocol=true (CVE-2024-53858 hardening): %+v", call.args)
		}
	}
	if strings.Contains(joined.String(), "SECRET-GITHUB-TOKEN") {
		t.Fatalf("git argv/env leaked token: %q", joined.String())
	}
	firstEnv := runner.calls[0].env
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false", "GCM_INTERACTIVE=never", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"} {
		if !containsArg(firstEnv, want) {
			t.Fatalf("git env missing %s: %+v", want, firstEnv)
		}
	}
}

func TestGitHubPrepareMissingRepoWithoutCreateFailsBeforeGit(t *testing.T) {
	repos := &testGitHubRepos{getErr: ErrGitHubRepoNotFound}
	runner := &testGitRunner{}
	backend := &GitHub{
		Root:        t.TempDir(),
		Config:      GitHubConfig{Host: "github.com", Owner: "octo", Repo: "demo"},
		Credentials: testGitHubCreds{},
		Repos:       repos,
		Git:         runner,
	}

	if _, err := backend.Prepare(context.Background(), "sp1", "main", t.TempDir(), -1); err == nil {
		t.Fatal("Prepare returned nil, want missing repo error")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("git calls = %+v, want none", runner.calls)
	}
}

func TestGitHubPrepareRejectsSplicedCloneURLBeforeGit(t *testing.T) {
	runner := &testGitRunner{}
	backend := &GitHub{
		Root:        t.TempDir(),
		Config:      GitHubConfig{Host: "github.com", Owner: "octo", Repo: "demo"},
		Credentials: testGitHubCreds{},
		Repos:       &testGitHubRepos{url: "https://token@github.com/octo/other.git"},
		Git:         runner,
	}

	if _, err := backend.Prepare(context.Background(), "sp1", "main", t.TempDir(), -1); err == nil {
		t.Fatal("Prepare returned nil, want clone_url validation error")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("git calls = %+v, want none", runner.calls)
	}
}

func TestGitHubPrepareCleansNodeOnlyHomeOnCloneFailure(t *testing.T) {
	root := t.TempDir()
	runner := &testGitRunner{failArg: "clone"}
	backend := &GitHub{
		Root:        root,
		Config:      GitHubConfig{Host: "github.com", Owner: "octo", Repo: "demo"},
		Credentials: testGitHubCreds{},
		Repos:       &testGitHubRepos{url: "https://github.com/octo/demo.git"},
		Git:         runner,
	}

	if _, err := backend.Prepare(context.Background(), "sp1", "main", t.TempDir(), -1); err == nil {
		t.Fatal("Prepare returned nil, want clone failure")
	}
	if _, err := os.Stat(filepath.Join(root, "github", "sp1", "main")); !os.IsNotExist(err) {
		t.Fatalf("host clone dir still exists after failure, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "github-home", "sp1", "main")); !os.IsNotExist(err) {
		t.Fatalf("node-only HOME still exists after failure, stat err=%v", err)
	}
}

func TestGitHubPrepareRepositoryIDMismatchFailsBeforeGit(t *testing.T) {
	runner := &testGitRunner{}
	backend := &GitHub{
		Root:        t.TempDir(),
		Config:      GitHubConfig{Host: "github.com", Owner: "octo", Repo: "demo", RepositoryID: "42"},
		Credentials: testGitHubCreds{},
		Repos:       &testGitHubRepos{url: "https://github.com/octo/demo.git", id: 999},
		Git:         runner,
	}
	_, err := backend.Prepare(context.Background(), "sp1", "main", t.TempDir(), -1)
	if !errors.Is(err, ErrGitHubRepoIDMismatch) {
		t.Fatalf("Prepare err = %v, want ErrGitHubRepoIDMismatch", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("git calls = %+v, want none (audit fails before clone)", runner.calls)
	}
}

func TestGitHubPrepareRepositoryIDMatchClonesExisting(t *testing.T) {
	runner := &testGitRunner{}
	backend := &GitHub{
		Root:        t.TempDir(),
		Config:      GitHubConfig{Host: "github.com", Owner: "octo", Repo: "demo", RepositoryID: "42"},
		Credentials: testGitHubCreds{},
		Repos:       &testGitHubRepos{url: "https://github.com/octo/demo.git", id: 42},
		Git:         runner,
	}
	if _, err := backend.Prepare(context.Background(), "sp1", "main", t.TempDir(), -1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(runner.calls) != 1 || !containsArg(runner.calls[0].args, "clone") {
		t.Fatalf("git calls = %+v, want a single clone", runner.calls)
	}
}

// TestGitHubPrepareClone2LeakHardening verifies CVE-2024-53858 / clone2leak protections:
//   - credential.protectProtocol=true is set on the clone command
//   - credential.helper= (empty reset) appears before the actual helper
//   - --recurse-submodules is NOT passed (node does not process untrusted .gitmodules)
func TestGitHubPrepareClone2LeakHardening(t *testing.T) {
	runner := &testGitRunner{}
	backend := &GitHub{
		Root:        t.TempDir(),
		Config:      GitHubConfig{Host: "github.com", Owner: "octo", Repo: "demo"},
		Credentials: testGitHubCreds{},
		Repos:       &testGitHubRepos{url: "https://github.com/octo/demo.git"},
		Git:         runner,
	}
	if _, err := backend.Prepare(context.Background(), "sp1", "main", t.TempDir(), -1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(runner.calls) == 0 {
		t.Fatal("no git calls recorded")
	}
	cloneCall := runner.calls[0]
	if !containsArg(cloneCall.args, "clone") {
		t.Fatalf("first git call is not clone: %+v", cloneCall.args)
	}
	// CVE-2024-53858: must refuse credential delivery on protocol downgrade.
	if !containsArg(cloneCall.args, "credential.protectProtocol=true") {
		t.Fatalf("clone missing credential.protectProtocol=true (CVE-2024-53858): %+v", cloneCall.args)
	}
	// Must reset credential.helper before setting ours, preventing any previously configured helper.
	if !containsArg(cloneCall.args, "credential.helper=") {
		t.Fatalf("clone missing credential.helper= (empty reset): %+v", cloneCall.args)
	}
	// Node must NOT clone with --recurse-submodules: .gitmodules from untrusted repos must not
	// be processed by node-side git ops (F22 / §16.8 residual boundary).
	for _, arg := range cloneCall.args {
		if arg == "--recurse-submodules" || arg == "--recurse_submodules" {
			t.Fatalf("clone must not pass --recurse-submodules (processes untrusted .gitmodules): %+v", cloneCall.args)
		}
	}
}

func TestGitHubPrepareCreatedRepoCoverageFailureFailsTyped(t *testing.T) {
	runner := &testGitRunner{}
	backend := &GitHub{
		Root: t.TempDir(),
		Config: GitHubConfig{
			Host: "github.com", Owner: "octo", Repo: "demo", CreateIfMissing: true,
		},
		Credentials: testGitHubCreds{},
		Repos: &testGitHubRepos{
			getErr:      ErrGitHubRepoNotFound,
			coverageErr: errors.New("403 not covered by installation"),
			url:         "https://github.com/octo/demo.git",
		},
		Git: runner,
	}
	_, err := backend.Prepare(context.Background(), "sp1", "main", t.TempDir(), -1)
	if !errors.Is(err, ErrGitHubRepoNotCovered) {
		t.Fatalf("Prepare err = %v, want ErrGitHubRepoNotCovered", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("git calls = %+v, want none (coverage fails before clone)", runner.calls)
	}
}

func TestGitHubPrepareSkipsCloneWhenRestorePending(t *testing.T) {
	root := t.TempDir()
	repos := &testGitHubRepos{getErr: errors.New("repos must not be consulted on restore")}
	runner := &testGitRunner{}
	backend := &GitHub{
		Root:        root,
		Config:      GitHubConfig{Host: "github.com", Owner: "octo", Repo: "demo", CreateIfMissing: true},
		Credentials: testGitHubCreds{},
		Repos:       repos,
		Git:         runner,
	}
	backend.SetRestorePending(true)

	hostDir, err := backend.Prepare(context.Background(), "sp1", "main", t.TempDir(), -1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if repos.getCalls != 0 {
		t.Fatalf("repos consulted %d times on restore, want 0", repos.getCalls)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("git calls = %+v, want none on restore", runner.calls)
	}
	entries, err := os.ReadDir(hostDir)
	if err != nil {
		t.Fatalf("read hostDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("hostDir not empty for journal to restore into: %+v", entries)
	}
	// node-only HOME must NOT be created when no git runs
	if _, serr := os.Stat(filepath.Join(root, "github-home", "sp1", "main")); !os.IsNotExist(serr) {
		t.Fatalf("github-home should not exist on restore-skip, stat=%v", serr)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
