package githubcred

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderExactRepoHelperOnlyAnswersForBoundRepo(t *testing.T) {
	root := t.TempDir()
	rendered, err := Render(root, RenderRequest{
		Host:        "github.com",
		Owner:       "octo-org",
		Repo:        "demo",
		Login:       "octocat",
		AccessToken: "ghu_live_token",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := runHelper(t, rendered.CredentialHelperPath, "protocol=https\nhost=github.com\npath=octo-org/demo.git\n\n")
	if !strings.Contains(got, "username=x-access-token\n") || !strings.Contains(got, "password=ghu_live_token\n") {
		t.Fatalf("helper output for bound repo = %q, want username and token", got)
	}

	for _, input := range []string{
		"protocol=https\nhost=github.com\npath=octo-org/other.git\n\n",
		"protocol=https\nhost=github.com\npath=other-org/demo.git\n\n",
		"protocol=https\nhost=github.example\npath=octo-org/demo.git\n\n",
		"protocol=http\nhost=github.com\npath=octo-org/demo.git\n\n",
	} {
		if out := runHelper(t, rendered.CredentialHelperPath, input); out != "" {
			t.Fatalf("helper output for non-bound input %q = %q, want empty", input, out)
		}
	}
}

func TestRenderWritesJournalExcludedMaterialUnderRoot(t *testing.T) {
	root := t.TempDir()
	rendered, err := Render(root, RenderRequest{
		Host:        "github.com",
		Owner:       "octo-org",
		Repo:        "demo",
		Login:       "octocat",
		AccessToken: "token-one",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, path := range []string{rendered.GHConfigDir, rendered.HostsPath, rendered.GitConfigPath, rendered.CredentialHelperPath, rendered.TokenPath} {
		if rel, err := filepath.Rel(root, path); err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			t.Fatalf("rendered path %q is not under root %q (rel=%q err=%v)", path, root, rel, err)
		}
	}

	hosts, err := os.ReadFile(rendered.HostsPath)
	if err != nil {
		t.Fatalf("read hosts.yml: %v", err)
	}
	if bytes.Contains(hosts, []byte("token-one")) {
		t.Fatalf("hosts.yml contains raw access token: %q", hosts)
	}
	if bytes.Contains(hosts, []byte("oauth_token:")) {
		t.Fatalf("hosts.yml contains oauth_token credential material: %q", hosts)
	}

	cfg, err := os.ReadFile(rendered.GitConfigPath)
	if err != nil {
		t.Fatalf("read git config: %v", err)
	}
	if !bytes.Contains(cfg, []byte("[credential]\n")) || !bytes.Contains(cfg, []byte("\tuseHttpPath = true\n")) {
		t.Fatalf("git config missing useHttpPath: %q", cfg)
	}
	if !bytes.Contains(cfg, []byte(rendered.CredentialHelperPath)) {
		t.Fatalf("git config missing helper path %q: %q", rendered.CredentialHelperPath, cfg)
	}

	tokenInfo, err := os.Stat(rendered.TokenPath)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if tokenInfo.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, want 0600", tokenInfo.Mode().Perm())
	}
	helperInfo, err := os.Stat(rendered.CredentialHelperPath)
	if err != nil {
		t.Fatalf("stat helper: %v", err)
	}
	if helperInfo.Mode().Perm() != 0o700 {
		t.Fatalf("helper mode = %v, want 0700", helperInfo.Mode().Perm())
	}
}

func TestRenderAtomicallyUpdatesTokenForRotationPickup(t *testing.T) {
	root := t.TempDir()
	rendered, err := Render(root, RenderRequest{
		Host:        "github.com",
		Owner:       "octo-org",
		Repo:        "demo",
		Login:       "octocat",
		AccessToken: "token-one",
	})
	if err != nil {
		t.Fatalf("Render first: %v", err)
	}
	if out := runHelper(t, rendered.CredentialHelperPath, "protocol=https\nhost=github.com\npath=octo-org/demo.git\n\n"); !strings.Contains(out, "password=token-one\n") {
		t.Fatalf("first helper output = %q, want token-one", out)
	}

	rendered2, err := Render(root, RenderRequest{
		Host:        "github.com",
		Owner:       "octo-org",
		Repo:        "demo",
		Login:       "octocat",
		AccessToken: "token-two",
	})
	if err != nil {
		t.Fatalf("Render second: %v", err)
	}
	if rendered2.TokenPath != rendered.TokenPath || rendered2.CredentialHelperPath != rendered.CredentialHelperPath {
		t.Fatalf("rotation changed paths: first=%+v second=%+v", rendered, rendered2)
	}
	if out := runHelper(t, rendered.CredentialHelperPath, "protocol=https\nhost=github.com\npath=octo-org/demo.git\n\n"); !strings.Contains(out, "password=token-two\n") {
		t.Fatalf("second helper output = %q, want token-two", out)
	}
}

func runHelper(t *testing.T, helperPath string, input string) string {
	t.Helper()
	cmd := exec.Command(helperPath, "get")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper %s failed: %v", helperPath, err)
	}
	return string(out)
}
