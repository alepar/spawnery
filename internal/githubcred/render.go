// Package githubcred renders GitHub credentials into journal-excluded tmpfs roots.
package githubcred

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultTargetDir = "github"
	defaultHost      = "github.com"
)

type RenderRequest struct {
	Host        string
	Owner       string
	Repo        string
	Login       string
	AccessToken string

	// RootInsideContainer is the path the same root is mounted at inside the agent. When empty,
	// helper/config files use root, which keeps host-side tests executable.
	RootInsideContainer string

	TargetDir            string
	GHConfigDir          string
	HostsPath            string
	GitConfigPath        string
	CredentialHelperPath string
	TokenPath            string
}

type Rendered struct {
	GHConfigDir          string
	HostsPath            string
	GitConfigPath        string
	CredentialHelperPath string
	TokenPath            string

	ContainerGHConfigDir          string
	ContainerGitConfigPath        string
	ContainerCredentialHelperPath string
	ContainerTokenPath            string
}

func Render(root string, req RenderRequest) (Rendered, error) {
	if root == "" {
		return Rendered{}, fmt.Errorf("github credential render root is required")
	}
	host := req.Host
	if host == "" {
		host = defaultHost
	}
	if host != defaultHost {
		return Rendered{}, fmt.Errorf("unsupported GitHub host %q", host)
	}
	if req.Owner == "" || req.Repo == "" {
		return Rendered{}, fmt.Errorf("github owner and repo are required")
	}
	if req.Login == "" {
		req.Login = "x-access-token"
	}
	if req.AccessToken == "" {
		return Rendered{}, fmt.Errorf("github access token is required")
	}

	targetDir := req.TargetDir
	if targetDir == "" {
		targetDir = defaultTargetDir
	}
	ghDir := req.GHConfigDir
	if ghDir == "" {
		ghDir = filepath.Join(targetDir, "gh")
	}
	hostsPath := req.HostsPath
	if hostsPath == "" {
		hostsPath = filepath.Join(ghDir, "hosts.yml")
	}
	gitConfigPath := req.GitConfigPath
	if gitConfigPath == "" {
		gitConfigPath = filepath.Join(targetDir, "gitconfig")
	}
	helperPath := req.CredentialHelperPath
	if helperPath == "" {
		helperPath = filepath.Join(targetDir, "git-credential-spawnery")
	}
	tokenPath := req.TokenPath
	if tokenPath == "" {
		tokenPath = filepath.Join(targetDir, "token")
	}

	rendered, err := resolveRenderedPaths(root, req.RootInsideContainer, ghDir, hostsPath, gitConfigPath, helperPath, tokenPath)
	if err != nil {
		return Rendered{}, err
	}
	for _, dir := range []string{
		rendered.GHConfigDir,
		filepath.Dir(rendered.HostsPath),
		filepath.Dir(rendered.GitConfigPath),
		filepath.Dir(rendered.CredentialHelperPath),
		filepath.Dir(rendered.TokenPath),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Rendered{}, err
		}
	}

	if err := writeFileAtomic(rendered.TokenPath, []byte(req.AccessToken+"\n"), 0o600); err != nil {
		return Rendered{}, err
	}
	hosts := fmt.Sprintf("%s:\n    user: %s\n    git_protocol: https\n", host, req.Login)
	if err := writeFileAtomic(rendered.HostsPath, []byte(hosts), 0o600); err != nil {
		return Rendered{}, err
	}
	// credential.protectProtocol=true refuses credentials when the protocol is downgraded from
	// https to http — clone2leak / CVE-2024-53858 hardening (§16.8).
	cfg := fmt.Sprintf("[credential]\n\thelper = %s\n\tuseHttpPath = true\n\tprotectProtocol = true\n", rendered.ContainerCredentialHelperPath)
	if err := writeFileAtomic(rendered.GitConfigPath, []byte(cfg), 0o600); err != nil {
		return Rendered{}, err
	}
	helper := helperScript(host, req.Owner, req.Repo, rendered.ContainerTokenPath)
	if err := writeFileAtomic(rendered.CredentialHelperPath, []byte(helper), 0o700); err != nil {
		return Rendered{}, err
	}
	return rendered, nil
}

func resolveRenderedPaths(root, containerRoot, ghDir, hostsPath, gitConfigPath, helperPath, tokenPath string) (Rendered, error) {
	hostRoot, err := filepath.Abs(root)
	if err != nil {
		return Rendered{}, err
	}
	if containerRoot == "" {
		containerRoot = hostRoot
	}
	resolve := func(rel string) (hostPath, containerPath string, err error) {
		if rel == "" {
			return "", "", fmt.Errorf("empty render path")
		}
		if filepath.IsAbs(rel) || strings.Contains(rel, "\x00") {
			return "", "", fmt.Errorf("render path %q must be relative", rel)
		}
		clean := filepath.Clean(rel)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", "", fmt.Errorf("render path %q escapes root", rel)
		}
		return filepath.Join(hostRoot, clean), filepath.Join(containerRoot, clean), nil
	}

	out := Rendered{}
	if out.GHConfigDir, out.ContainerGHConfigDir, err = resolve(ghDir); err != nil {
		return Rendered{}, err
	}
	if out.HostsPath, _, err = resolve(hostsPath); err != nil {
		return Rendered{}, err
	}
	if out.GitConfigPath, out.ContainerGitConfigPath, err = resolve(gitConfigPath); err != nil {
		return Rendered{}, err
	}
	if out.CredentialHelperPath, out.ContainerCredentialHelperPath, err = resolve(helperPath); err != nil {
		return Rendered{}, err
	}
	if out.TokenPath, out.ContainerTokenPath, err = resolve(tokenPath); err != nil {
		return Rendered{}, err
	}
	return out, nil
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return os.Chmod(path, mode)
}

func helperScript(host, owner, repo, tokenPath string) string {
	return fmt.Sprintf(`#!/bin/sh
protocol=""
host=""
path=""
while IFS='=' read -r key value; do
	case "$key" in
	protocol) protocol="$value" ;;
	host) host="$value" ;;
	path) path="$value" ;;
	esac
done

case "$path" in
%[3]s/%[4]s) ;;
%[3]s/%[4]s.git) ;;
*) exit 0 ;;
esac

if [ "$protocol" != "https" ] || [ "$host" != "%[2]s" ]; then
	exit 0
fi

token="$(cat "%[1]s" 2>/dev/null || true)"
token="${token%%'
'*}"
if [ -z "$token" ]; then
	exit 0
fi
printf 'username=x-access-token\n'
printf 'password=%%s\n' "$token"
`, shellQuote(tokenPath), host, owner, repo)
}

func shellQuote(s string) string {
	return strings.ReplaceAll(s, `'`, `'\''`)
}
