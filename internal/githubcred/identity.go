package githubcred

import (
	"fmt"
	"strconv"
	"strings"
)

// Identity is the resolved git commit identity for the gitconfig [user] section.
type Identity struct{ Name, Email string }

// ResolveIdentity computes the commit identity from the linked GitHub account's login and numeric user
// id (design §1.2). Canonical form requires BOTH a login and a non-zero id; otherwise it falls back to
// a spawnery.local noreply address. Both fields are always non-empty.
func ResolveIdentity(login string, userID int64) Identity {
	login = strings.TrimSpace(login)
	if login != "" && userID > 0 {
		return Identity{Name: login, Email: fmt.Sprintf("%d+%s@users.noreply.github.com", userID, login)}
	}
	name := login
	if name == "" {
		name = "spawnery"
	}
	account := name
	if userID > 0 {
		account = strconv.FormatInt(userID, 10)
	}
	return Identity{Name: name, Email: account + "@users.noreply.spawnery.local"}
}

// RenderIdentity writes a global gitconfig at gitConfigPath containing only the [user] section seeded
// from id (design §1.2). Mode 0644 so the agent (which owns the git-env dir) can still override it via
// `git config --global`. Parent dirs are created; the write is atomic.
func RenderIdentity(gitConfigPath string, id Identity) error {
	if gitConfigPath == "" {
		return fmt.Errorf("git config path is required")
	}
	cfg := fmt.Sprintf("[user]\n\tname = %s\n\temail = %s\n", id.Name, id.Email)
	return writeFileAtomic(gitConfigPath, []byte(cfg), 0o644)
}
