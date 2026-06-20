package githubcred

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveIdentity(t *testing.T) {
	tests := []struct {
		login     string
		userID    int64
		wantName  string
		wantEmail string
	}{
		// Canonical: both login and userID present.
		{"octocat", 583231, "octocat", "583231+octocat@users.noreply.github.com"},
		// Fallback: login present, userID == 0.
		{"octocat", 0, "octocat", "octocat@users.noreply.spawnery.local"},
		// Fallback: login empty, userID present.
		{"", 42, "spawnery", "42@users.noreply.spawnery.local"},
		// Fallback: both empty.
		{"", 0, "spawnery", "spawnery@users.noreply.spawnery.local"},
	}
	for _, tt := range tests {
		t.Run(tt.login+"_"+strings.ReplaceAll(tt.wantEmail, "@", "_at_"), func(t *testing.T) {
			got := ResolveIdentity(tt.login, tt.userID)
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if got.Email != tt.wantEmail {
				t.Errorf("Email = %q, want %q", got.Email, tt.wantEmail)
			}
		})
	}
}

func TestRenderIdentityWritesUserSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitconfig")
	id := Identity{Name: "octocat", Email: "583231+octocat@users.noreply.github.com"}
	if err := RenderIdentity(path, id); err != nil {
		t.Fatalf("RenderIdentity: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "[user]\n\tname = octocat\n\temail = 583231+octocat@users.noreply.github.com\n"
	if string(b) != want {
		t.Errorf("content = %q, want %q", string(b), want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", info.Mode().Perm())
	}
}

func TestRenderIdentityGitParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitconfig")
	id := Identity{Name: "octocat", Email: "583231+octocat@users.noreply.github.com"}
	if err := RenderIdentity(path, id); err != nil {
		t.Fatalf("RenderIdentity: %v", err)
	}
	// Use git to verify the file is parseable.
	out, err := exec.Command("git", "config", "--file", path, "user.email").Output()
	if err != nil {
		t.Fatalf("git config: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != id.Email {
		t.Errorf("git config user.email = %q, want %q", got, id.Email)
	}
}

func TestRenderIdentityIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitconfig")
	id := Identity{Name: "octocat", Email: "583231+octocat@users.noreply.github.com"}
	if err := RenderIdentity(path, id); err != nil {
		t.Fatalf("first RenderIdentity: %v", err)
	}
	b1, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile first: %v", err)
	}
	if err := RenderIdentity(path, id); err != nil {
		t.Fatalf("second RenderIdentity: %v", err)
	}
	b2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile second: %v", err)
	}
	if string(b1) != string(b2) {
		t.Errorf("idempotent: first=%q second=%q", string(b1), string(b2))
	}
}
