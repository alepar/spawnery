package agentinstall_test

import (
	"path/filepath"
	"testing"

	"spawnery/internal/agentinstall"
)

func TestRegistryNames(t *testing.T) {
	env := agentinstall.MapEnviron{"HOME": "/home/test"}
	reg := agentinstall.NewRegistry(env)
	names := reg.Names()
	want := []string{"claude", "codex", "opencode", "hermes", "goose"}
	if len(names) != len(want) {
		t.Fatalf("Names() = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestRegistryLookup(t *testing.T) {
	env := agentinstall.MapEnviron{"HOME": "/home/test"}
	reg := agentinstall.NewRegistry(env)

	for _, name := range []string{"claude", "codex", "opencode", "hermes", "goose"} {
		e, ok := reg.Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) not found", name)
			continue
		}
		if e.Layout().Name != name {
			t.Errorf("Lookup(%q).Layout().Name = %q, want %q", name, e.Layout().Name, name)
		}
	}

	_, ok := reg.Lookup("unknown-agent")
	if ok {
		t.Error("Lookup(unknown-agent) should return not-found")
	}
}

func TestRegistryLayouts(t *testing.T) {
	home := "/home/testuser"
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	layouts := reg.Layouts()

	if len(layouts) != 5 {
		t.Fatalf("expected 5 layouts, got %d", len(layouts))
	}

	// Build a map for easier lookup.
	lm := make(map[string]agentinstall.AgentLayout)
	for _, l := range layouts {
		lm[l.Name] = l
	}

	t.Run("claude", func(t *testing.T) {
		l := lm["claude"]
		assertPath(t, "ConfigRoot", l.ConfigRoot, filepath.Join(home, ".claude"))
		assertPath(t, "SkillPath", l.SkillPath, filepath.Join(home, ".claude", "skills"))
		assertPath(t, "MCPPath", l.MCPPath, filepath.Join(home, ".claude.json"))
		assertFormat(t, "MCPFormat", l.MCPFormat, agentinstall.FormatJSON)
		assertPath(t, "ConfigPath", l.ConfigPath, filepath.Join(home, ".claude", "settings.json"))
		assertFormat(t, "ConfigFormat", l.ConfigFormat, agentinstall.FormatJSON)
		assertForbidden(t, l.ForbiddenConfigKeys, "model")
	})

	t.Run("codex", func(t *testing.T) {
		l := lm["codex"]
		assertPath(t, "ConfigRoot", l.ConfigRoot, filepath.Join(home, ".codex"))
		assertPath(t, "SkillPath", l.SkillPath, filepath.Join(home, ".codex", "skills"))
		assertPath(t, "MCPPath", l.MCPPath, filepath.Join(home, ".codex", "config.toml"))
		assertFormat(t, "MCPFormat", l.MCPFormat, agentinstall.FormatTOML)
		assertPath(t, "ConfigPath", l.ConfigPath, filepath.Join(home, ".codex", "config.toml"))
		assertFormat(t, "ConfigFormat", l.ConfigFormat, agentinstall.FormatTOML)
	})

	t.Run("opencode", func(t *testing.T) {
		l := lm["opencode"]
		assertPath(t, "ConfigRoot", l.ConfigRoot, filepath.Join(home, ".config", "opencode"))
		assertPath(t, "MCPPath", l.MCPPath, filepath.Join(home, ".config", "opencode", "opencode.json"))
		assertFormat(t, "MCPFormat", l.MCPFormat, agentinstall.FormatJSONC)
		assertPath(t, "ConfigPath", l.ConfigPath, filepath.Join(home, ".config", "opencode", "opencode.json"))
		assertFormat(t, "ConfigFormat", l.ConfigFormat, agentinstall.FormatJSONC)
	})

	t.Run("hermes", func(t *testing.T) {
		l := lm["hermes"]
		assertPath(t, "ConfigRoot", l.ConfigRoot, filepath.Join(home, ".hermes"))
		assertPath(t, "SkillPath", l.SkillPath, filepath.Join(home, ".agents", "skills"))
		assertPath(t, "MCPPath", l.MCPPath, filepath.Join(home, ".hermes", "config.yaml"))
		assertFormat(t, "MCPFormat", l.MCPFormat, agentinstall.FormatYAML)
		assertPath(t, "ConfigPath", l.ConfigPath, filepath.Join(home, ".hermes", "config.yaml"))
		assertFormat(t, "ConfigFormat", l.ConfigFormat, agentinstall.FormatYAML)
	})

	t.Run("goose", func(t *testing.T) {
		l := lm["goose"]
		assertPath(t, "ConfigRoot", l.ConfigRoot, filepath.Join(home, ".config", "goose"))
		assertPath(t, "MCPPath", l.MCPPath, filepath.Join(home, ".config", "goose", "config.yaml"))
		assertFormat(t, "MCPFormat", l.MCPFormat, agentinstall.FormatYAML)
		assertPath(t, "ConfigPath", l.ConfigPath, filepath.Join(home, ".config", "goose", "config.yaml"))
		assertFormat(t, "ConfigFormat", l.ConfigFormat, agentinstall.FormatYAML)
	})
}

func TestRegistryLayoutsWithCodexHome(t *testing.T) {
	home := "/home/testuser"
	codexHome := "/opt/codex"
	env := agentinstall.MapEnviron{
		"HOME":       home,
		"CODEX_HOME": codexHome,
	}
	reg := agentinstall.NewRegistry(env)
	layouts := reg.Layouts()

	lm := make(map[string]agentinstall.AgentLayout)
	for _, l := range layouts {
		lm[l.Name] = l
	}

	l := lm["codex"]
	assertPath(t, "ConfigRoot", l.ConfigRoot, codexHome)
	assertPath(t, "SkillPath", l.SkillPath, filepath.Join(codexHome, "skills"))
	assertPath(t, "MCPPath", l.MCPPath, filepath.Join(codexHome, "config.toml"))
}

func TestRegistryLayoutsWithXDGConfigHome(t *testing.T) {
	home := "/home/testuser"
	xdgConfig := "/custom/config"
	env := agentinstall.MapEnviron{
		"HOME":            home,
		"XDG_CONFIG_HOME": xdgConfig,
	}
	reg := agentinstall.NewRegistry(env)
	layouts := reg.Layouts()

	lm := make(map[string]agentinstall.AgentLayout)
	for _, l := range layouts {
		lm[l.Name] = l
	}

	t.Run("opencode", func(t *testing.T) {
		l := lm["opencode"]
		assertPath(t, "ConfigRoot", l.ConfigRoot, filepath.Join(xdgConfig, "opencode"))
		assertPath(t, "MCPPath", l.MCPPath, filepath.Join(xdgConfig, "opencode", "opencode.json"))
	})

	t.Run("goose", func(t *testing.T) {
		l := lm["goose"]
		assertPath(t, "ConfigRoot", l.ConfigRoot, filepath.Join(xdgConfig, "goose"))
		assertPath(t, "MCPPath", l.MCPPath, filepath.Join(xdgConfig, "goose", "config.yaml"))
	})
}

func assertPath(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %q, want %q", field, got, want)
	}
}

func assertFormat(t *testing.T, field string, got, want agentinstall.Format) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %q, want %q", field, got, want)
	}
}

func assertForbidden(t *testing.T, keys []string, want string) {
	t.Helper()
	for _, k := range keys {
		if k == want {
			return
		}
	}
	t.Errorf("ForbiddenConfigKeys does not contain %q; got %v", want, keys)
}
