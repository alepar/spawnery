package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	ErrGitHubRepoNotFound   = errors.New("github: repository not found")
	ErrGitHubRepoIDMismatch = errors.New("github: bound repository_id does not match repository")
	ErrGitHubRepoNotCovered = errors.New("github: live token does not cover repository")
)

type GitHubConfig struct {
	Host               string
	Owner              string
	Repo               string
	MountName          string
	CredentialSecretID string
	CreateIfMissing    bool
	RepositoryID       string
	// AllowInsecureHost bypasses the github.com host restriction and allows HTTP
	// clone URLs. Set ONLY in tests against a local git server (e.g., gitea).
	// Production code must never set this field.
	AllowInsecureHost bool
}

type GitHubCredential struct {
	AccessToken          string
	TokenPath            string
	CredentialHelperPath string
}

func (c GitHubCredential) Token() (string, error) {
	if strings.TrimSpace(c.AccessToken) != "" {
		return strings.TrimSpace(c.AccessToken), nil
	}
	if c.TokenPath == "" {
		return "", fmt.Errorf("github credential has no access token or token path")
	}
	b, err := os.ReadFile(c.TokenPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

type GitHubCredentialProvider interface {
	TokenForGitHubMount(ctx context.Context, spawnID, mountName string, cfg GitHubConfig) (GitHubCredential, error)
}

type GitHubRepoInfo struct {
	CloneURL string
	Empty    bool
	ID       int64
}

type GitHubRepoService interface {
	Get(ctx context.Context, cfg GitHubConfig, token string) (GitHubRepoInfo, error)
	Create(ctx context.Context, cfg GitHubConfig, token string) (GitHubRepoInfo, error)
}

type GitRunner interface {
	RunGit(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)
}

type execGitRunner struct{}

func (execGitRunner) RunGit(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

type GitHub struct {
	Root        string
	Config      GitHubConfig
	Credentials GitHubCredentialProvider
	Repos       GitHubRepoService
	Git         GitRunner

	restorePending bool
}

// SetRestorePending implements storage.RestoreAware: when true, Prepare provides an empty
// agent-writable dir for the journaler to restore into and skips the network clone/create/seed.
func (g *GitHub) SetRestorePending(pending bool) { g.restorePending = pending }

func NewGitHub(root string, cfg GitHubConfig) *GitHub {
	return &GitHub{Root: root, Config: cfg}
}

func ParseGitHubURI(uri string) (GitHubConfig, error) {
	rest, ok := strings.CutPrefix(uri, "github:")
	if !ok {
		return GitHubConfig{}, fmt.Errorf("github backend URI must start with github:")
	}
	rest = strings.TrimPrefix(rest, "//")
	rest = strings.Trim(rest, "/")
	owner, repo, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return GitHubConfig{}, fmt.Errorf("invalid github backend URI %q: want github:owner/repo", uri)
	}
	repo = strings.TrimSuffix(repo, ".git")
	if repo == "" {
		return GitHubConfig{}, fmt.Errorf("invalid github backend URI %q: empty repo", uri)
	}
	return GitHubConfig{Host: "github.com", Owner: owner, Repo: repo}, nil
}

// validateRepositoryID is an AUDIT check, not a scope guarantee (invariant e): the bound
// repository_id must equal the id of the repo actually reached for {owner,repo}. A mismatch means
// the path was renamed/recreated/confused; fail closed. A zero/absent id from the service or an
// empty binding is audit-skipped (the installation selection remains the only scope guarantee).
func validateRepositoryID(cfg GitHubConfig, info GitHubRepoInfo) error {
	want := strings.TrimSpace(cfg.RepositoryID)
	if want == "" || info.ID == 0 {
		return nil
	}
	if strconv.FormatInt(info.ID, 10) != want {
		return fmt.Errorf("%w: bound %s, reached %d for %s/%s",
			ErrGitHubRepoIDMismatch, want, info.ID, cfg.Owner, cfg.Repo)
	}
	return nil
}

func (g *GitHub) Prepare(ctx context.Context, spawnID, mountName, seedDir string, agentUID int) (string, error) {
	cfg := g.Config
	if cfg.MountName == "" {
		cfg.MountName = mountName
	}
	if cfg.Host != "github.com" && !cfg.AllowInsecureHost {
		return "", fmt.Errorf("github unsupported host %q", cfg.Host)
	}

	hostDir := filepath.Join(g.Root, "github", spawnID, mountName)

	if g.restorePending {
		// Resume (spec §16.7): a journal restore will repopulate hostDir; the journal is authoritative.
		// Provide a clean, agent-writable empty dir and skip all network/git — no token mint, no clone.
		if err := os.RemoveAll(hostDir); err != nil {
			return "", err
		}
		if err := os.MkdirAll(hostDir, 0o755); err != nil {
			return "", err
		}
		if err := NormalizeOwnership(hostDir, agentUID); err != nil {
			return "", err
		}
		return hostDir, nil
	}

	if g.Credentials == nil {
		return "", fmt.Errorf("github credential provider is not configured")
	}
	cred, err := g.Credentials.TokenForGitHubMount(ctx, spawnID, mountName, cfg)
	if err != nil {
		return "", fmt.Errorf("github credentials for mount %q: %w", mountName, err)
	}
	token, err := cred.Token()
	if err != nil {
		return "", fmt.Errorf("github token for mount %q: %w", mountName, err)
	}

	repos := g.Repos
	if repos == nil {
		repos = defaultGitHubRepoService{}
	}
	created := false
	info, err := repos.Get(ctx, cfg, token)
	if errors.Is(err, ErrGitHubRepoNotFound) {
		if !cfg.CreateIfMissing {
			return "", fmt.Errorf("github repo %s/%s not found and create_if_missing is false", cfg.Owner, cfg.Repo)
		}
		info, err = repos.Create(ctx, cfg, token)
		created = true
	}
	if err != nil {
		return "", fmt.Errorf("github repo %s/%s: %w", cfg.Owner, cfg.Repo, err)
	}
	if created {
		// Spec §8 step 6: confirm the live installation-selection-scoped token actually reaches the
		// freshly created repo (the spike showed selected-repo installs cover it immediately) BEFORE
		// seeding. Coverage is the install selection, not repository_id (invariant e).
		covInfo, cerr := repos.Get(ctx, cfg, token)
		if cerr != nil {
			return "", fmt.Errorf("%w: %s/%s: %v", ErrGitHubRepoNotCovered, cfg.Owner, cfg.Repo, cerr)
		}
		info = covInfo
	} else if verr := validateRepositoryID(cfg, info); verr != nil {
		return "", verr
	}
	if info.CloneURL == "" {
		info.CloneURL = fmt.Sprintf("https://github.com/%s/%s.git", cfg.Owner, cfg.Repo)
	}
	if err := validateGitHubCloneURL(info.CloneURL, cfg); err != nil {
		return "", err
	}

	homeDir := filepath.Join(g.Root, "github-home", spawnID, mountName)
	cleanupDirs := func() {
		_ = os.RemoveAll(hostDir)
		_ = os.RemoveAll(homeDir)
	}
	if err := os.RemoveAll(hostDir); err != nil {
		return "", err
	}
	if err := os.RemoveAll(homeDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(hostDir), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return "", err
	}
	runner := g.Git
	if runner == nil {
		runner = execGitRunner{}
	}
	gitEnv := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"GCM_INTERACTIVE=never",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"HOME=" + homeDir,
	}
	// clone2leak / CVE-2024-53858 hardening (§16.8):
	//  - credential.helper= (empty) resets any repo-injected helper before our helper is set.
	//  - credential.useHttpPath=true ensures exact-repo matching in our helper (set by sp-u53.1.2).
	//  - credential.protectProtocol=true refuses credentials if the protocol is downgraded from
	//    https to http by a repo-injected config or a MITM redirect — prevents token exfiltration
	//    via protocol downgrade.
	// We never pass --recurse-submodules: the node does not process .gitmodules on untrusted repos,
	// so repo-injected credential.helper entries in .gitmodules and .lfsconfig are not evaluated
	// during node-side git ops. The agent's own git on hostile content is a documented residual
	// (bounded by the installation-selection blast radius; future: git proxy sp-51li).
	helperArgs := []string{
		"-c", "credential.helper=",
		"-c", "credential.useHttpPath=true",
		"-c", "credential.protectProtocol=true",
	}
	if cred.CredentialHelperPath != "" {
		helperArgs = append(helperArgs, "-c", "credential.helper="+cred.CredentialHelperPath)
	}
	args := append(append([]string{}, helperArgs...), "clone", "--", info.CloneURL, hostDir)
	if out, err := runner.RunGit(ctx, "", gitEnv, args...); err != nil {
		cleanupDirs()
		return "", fmt.Errorf("git clone %s/%s: %w (%s)", cfg.Owner, cfg.Repo, err, bytes.TrimSpace(out))
	}

	if info.Empty || userTreeEmpty(hostDir) {
		seeded, err := seedGitHubWorkingTree(seedDir, hostDir, agentUID)
		if err != nil {
			cleanupDirs()
			return "", err
		}
		// A genuinely empty repo (no commits, per the GitHub API) has no branch: always establish
		// one with an initial commit and push it, so the agent has a main branch to work on and
		// suspend/resume has a base. initialCommitAndPush uses --allow-empty, so this succeeds even
		// with no seed. For an existing repo that merely cloned an empty tree, keep the prior
		// behavior of committing only when a seed contributed files.
		if info.Empty || seeded {
			if err := g.initialCommitAndPush(ctx, runner, gitEnv, helperArgs, hostDir); err != nil {
				cleanupDirs()
				return "", err
			}
		}
	}
	if err := NormalizeOwnership(hostDir, agentUID); err != nil {
		cleanupDirs()
		return "", err
	}
	return hostDir, nil
}

func (g *GitHub) initialCommitAndPush(ctx context.Context, runner GitRunner, env, helperArgs []string, hostDir string) error {
	commitArgs := append(append([]string{}, helperArgs...),
		"-c", "user.name=Spawnery",
		"-c", "user.email=spawnery@example.invalid",
		// --allow-empty: a github mount over an empty repo with no seed still gets a valid initial
		// commit so the repo has a main branch (the agent can commit onto it; resume has a base).
		"commit", "--allow-empty", "-m", "Initialize Spawnery mount",
	)
	commands := []struct {
		name string
		args []string
	}{
		{name: "add", args: append(append([]string{}, helperArgs...), "add", "-A")},
		{name: "commit", args: commitArgs},
		{name: "push", args: append(append([]string{}, helperArgs...), "push", "origin", "HEAD")},
	}
	for _, cmd := range commands {
		out, err := runner.RunGit(ctx, hostDir, env, cmd.args...)
		if err != nil {
			return fmt.Errorf("git %s: %w (%s)", cmd.name, err, bytes.TrimSpace(out))
		}
	}
	return nil
}

func seedGitHubWorkingTree(seedDir, hostDir string, agentUID int) (bool, error) {
	entries, err := os.ReadDir(seedDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if len(entries) == 0 {
		return false, nil
	}
	filePerm := os.FileMode(0o644)
	chownTo := agentUID
	if agentUID < 0 {
		chownTo = -1
		filePerm = 0o666
	}
	if err := copyDirFiles(seedDir, hostDir, filePerm, chownTo); err != nil {
		return false, err
	}
	return !userTreeEmpty(hostDir), nil
}

func userTreeEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	for _, entry := range entries {
		if entry.Name() != ".git" {
			return false
		}
	}
	return true
}

func validateGitHubCloneURL(raw string, cfg GitHubConfig) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("github clone_url %q: %w", raw, err)
	}
	// For production (github.com), enforce HTTPS to prevent token exposure over the wire.
	// AllowInsecureHost relaxes this for local test git servers (e.g., gitea over HTTP).
	schemeOK := u.Scheme == "https" || (cfg.AllowInsecureHost && u.Scheme == "http")
	if !schemeOK || u.Host != cfg.Host || u.User != nil {
		return fmt.Errorf("github clone_url %q does not match bound host", raw)
	}
	wantPath := "/" + cfg.Owner + "/" + cfg.Repo + ".git"
	if strings.TrimSuffix(u.EscapedPath(), ".git") != strings.TrimSuffix(wantPath, ".git") {
		return fmt.Errorf("github clone_url %q does not match bound repo %s/%s", raw, cfg.Owner, cfg.Repo)
	}
	return nil
}

func (g *GitHub) Finalize(_ context.Context, hostDir string) error {
	err := os.RemoveAll(hostDir)
	if rel, rerr := filepath.Rel(filepath.Join(g.Root, "github"), hostDir); rerr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		if herr := os.RemoveAll(filepath.Join(g.Root, "github-home", rel)); err == nil {
			err = herr
		}
	}
	return err
}

type defaultGitHubRepoService struct {
	Client  *http.Client
	BaseURL string
}

func (s defaultGitHubRepoService) Get(ctx context.Context, cfg GitHubConfig, token string) (GitHubRepoInfo, error) {
	var out struct {
		CloneURL string `json:"clone_url"`
		Size     int64  `json:"size"`
		ID       int64  `json:"id"`
	}
	if err := s.do(ctx, http.MethodGet, "/repos/"+cfg.Owner+"/"+cfg.Repo, token, nil, &out); err != nil {
		return GitHubRepoInfo{}, err
	}
	return GitHubRepoInfo{CloneURL: out.CloneURL, Empty: out.Size == 0, ID: out.ID}, nil
}

func (s defaultGitHubRepoService) Create(ctx context.Context, cfg GitHubConfig, token string) (GitHubRepoInfo, error) {
	body := map[string]any{
		"name":    cfg.Repo,
		"private": true,
	}
	var out struct {
		CloneURL string `json:"clone_url"`
		Size     int64  `json:"size"`
		ID       int64  `json:"id"`
	}
	if err := s.do(ctx, http.MethodPost, "/user/repos", token, body, &out); err != nil {
		return GitHubRepoInfo{}, err
	}
	return GitHubRepoInfo{CloneURL: out.CloneURL, Empty: out.Size == 0, ID: out.ID}, nil
}

func (s defaultGitHubRepoService) do(ctx context.Context, method, path, token string, body any, out any) error {
	base := s.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return ErrGitHubRepoNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("github api %s %s: status %d: %s%s", method, path, resp.StatusCode, bytes.TrimSpace(b), githubErrorDiag(resp.Header, token))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// githubErrorDiag extracts GitHub's permission-diagnostic response headers so a failed call is
// actionable instead of opaque. On a 403 "Resource not accessible by integration",
// X-Accepted-GitHub-Permissions states exactly which App/token permission GitHub required (e.g.
// "administration=write") — invaluable when the granted-installation permissions lag the App's
// registered set. token-type is the non-secret 4-char prefix (ghu_=user-to-server,
// ghs_=installation, gho_=oauth) — it never includes the secret. Returns "" when no diag is present.
func githubErrorDiag(h http.Header, token string) string {
	var parts []string
	if v := h.Get("X-Accepted-GitHub-Permissions"); v != "" {
		parts = append(parts, "accepted-permissions="+v)
	}
	if v := h.Get("X-Accepted-Oauth-Scopes"); v != "" {
		parts = append(parts, "accepted-oauth-scopes="+v)
	}
	if v := h.Get("X-Oauth-Scopes"); v != "" {
		parts = append(parts, "oauth-scopes="+v)
	}
	if v := h.Get("X-Github-Request-Id"); v != "" {
		parts = append(parts, "request-id="+v)
	}
	if len(token) >= 4 {
		parts = append(parts, "token-type="+token[:4])
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, " ") + "]"
}
