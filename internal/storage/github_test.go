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
	getErr  error
	created bool
	url     string
}

func (r *testGitHubRepos) Get(context.Context, GitHubConfig, string) (GitHubRepoInfo, error) {
	if r.getErr != nil {
		return GitHubRepoInfo{}, r.getErr
	}
	return GitHubRepoInfo{CloneURL: r.url, Empty: false}, nil
}

func (r *testGitHubRepos) Create(context.Context, GitHubConfig, string) (GitHubRepoInfo, error) {
	r.created = true
	return GitHubRepoInfo{CloneURL: r.url, Empty: true}, nil
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

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
