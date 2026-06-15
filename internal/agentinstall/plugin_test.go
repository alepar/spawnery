package agentinstall_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"

	"spawnery/internal/agentinstall"
)

// readJSON is a test helper that reads and unmarshals a JSON file.
func readJSON(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readJSON: read %s: %v", path, err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("readJSON: unmarshal %s: %v", path, err)
	}
	return root
}

// readTOML is a test helper that reads and unmarshals a TOML file.
func readTOML(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readTOML: read %s: %v", path, err)
	}
	var root map[string]interface{}
	if err := toml.Unmarshal(data, &root); err != nil {
		t.Fatalf("readTOML: unmarshal %s: %v", path, err)
	}
	return root
}

// readJSONC is a test helper that reads and unmarshals a JSONC file via hujson.
func readJSONC(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readJSONC: read %s: %v", path, err)
	}
	std, err := hujson.Standardize(data)
	if err != nil {
		t.Fatalf("readJSONC: standardize %s: %v", path, err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(std, &root); err != nil {
		t.Fatalf("readJSONC: unmarshal %s: %v", path, err)
	}
	return root
}

// ---- claude ------------------------------------------------------------------

func TestClaudeInstallPlugin(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, _ := reg.Lookup("claude")
	a := agentinstall.Artifact{Kind: agentinstall.KindPlugin, Name: "demo",
		Plugin: &agentinstall.PluginPayload{Plugin: "p", Marketplace: "mp", Source: "/baked/mp"}}
	r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home})
	if r.Status != agentinstall.StatusApplied || r.Capability != agentinstall.CapabilityApplied {
		t.Fatalf("got %+v", r)
	}
	root := readJSON(t, filepath.Join(home, ".claude", "settings.json"))
	ep, ok := root["enabledPlugins"].(map[string]interface{})
	if !ok {
		t.Fatalf("enabledPlugins missing or wrong type: %v", root)
	}
	if ep["p@mp"] != true {
		t.Fatalf("enabledPlugins missing p@mp: %v", root)
	}
	mp, ok := root["extraKnownMarketplaces"].(map[string]interface{})
	if !ok {
		t.Fatalf("extraKnownMarketplaces missing or wrong type: %v", root)
	}
	if _, ok := mp["mp"]; !ok {
		t.Fatalf("extraKnownMarketplaces missing mp: %v", root)
	}
}

func TestClaudeInstallPlugin_NilPayload(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, _ := reg.Lookup("claude")
	a := agentinstall.Artifact{Kind: agentinstall.KindPlugin, Name: "demo"}
	r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home})
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("nil plugin payload should skip, got %+v", r)
	}
}

func TestClaudeInstallPlugin_InvalidIdent(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, _ := reg.Lookup("claude")
	a := agentinstall.Artifact{Kind: agentinstall.KindPlugin, Name: "demo",
		Plugin: &agentinstall.PluginPayload{Plugin: "p@bad", Marketplace: "mp"}}
	r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home})
	if r.Status != agentinstall.StatusFailed {
		t.Fatalf("invalid plugin name should fail, got %+v", r)
	}
}

// ---- codex -------------------------------------------------------------------

func TestCodexInstallPlugin_Local(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, _ := reg.Lookup("codex")
	a := agentinstall.Artifact{Kind: agentinstall.KindPlugin, Name: "demo",
		Plugin: &agentinstall.PluginPayload{Plugin: "p", Marketplace: "mp", Source: "/baked/mp"}}
	r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home})
	if r.Status != agentinstall.StatusApplied || r.Capability != agentinstall.CapabilityApplied {
		t.Fatalf("got %+v", r)
	}
	root := readTOML(t, filepath.Join(home, ".codex", "config.toml"))
	plugins, ok := root["plugins"].(map[string]interface{})
	if !ok {
		t.Fatalf("plugins missing or wrong type: %v", root)
	}
	if _, ok := plugins["p@mp"]; !ok {
		t.Fatalf("plugins missing p@mp: %v", root)
	}
}

func TestCodexInstallPlugin_OAuthNoOp(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, _ := reg.Lookup("codex")
	a := agentinstall.Artifact{Kind: agentinstall.KindPlugin, Name: "demo",
		Plugin: &agentinstall.PluginPayload{Plugin: "p", Marketplace: "mp", Source: "/baked/mp", RequiresOAuth: true}}
	r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home})
	if r.Status != agentinstall.StatusSkipped || r.Capability != agentinstall.CapabilityUnsupported {
		t.Fatalf("got %+v", r)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); err == nil {
		t.Fatalf("config.toml should not be written for OAuth no-op")
	}
}

// ---- opencode ----------------------------------------------------------------

func TestOpencodeInstallPlugin_Local(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, _ := reg.Lookup("opencode")
	a := agentinstall.Artifact{Kind: agentinstall.KindPlugin, Name: "demo",
		Plugin: &agentinstall.PluginPayload{Plugin: "p", Marketplace: "mp", LocalFile: "./plugins/p.js"}}
	if r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home}); r.Status != agentinstall.StatusApplied || r.Capability != agentinstall.CapabilityApplied {
		t.Fatalf("got %+v", r)
	}
	// second apply: no duplicate
	if r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home}); r.Status != agentinstall.StatusApplied {
		t.Fatalf("reapply got %+v", r)
	}
	root := readJSONC(t, filepath.Join(home, ".config", "opencode", "opencode.json"))
	arr, ok := root["plugin"].([]interface{})
	if !ok {
		t.Fatalf("plugin array missing or wrong type: %v", root)
	}
	if len(arr) != 1 || arr[0] != "./plugins/p.js" {
		t.Fatalf("plugin array: %v", arr)
	}
}

func TestOpencodeInstallPlugin_NPMBestEffort(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, _ := reg.Lookup("opencode")
	a := agentinstall.Artifact{Kind: agentinstall.KindPlugin, Name: "demo",
		Plugin: &agentinstall.PluginPayload{Plugin: "p", Marketplace: "mp", NPM: "opencode-plugin-foo@1.2.3"}}
	r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home})
	if r.Status != agentinstall.StatusApplied || r.Capability != agentinstall.CapabilityBestEffort {
		t.Fatalf("got %+v", r)
	}
	if r.Reason == "" {
		t.Fatalf("npm best-effort must carry a reason")
	}
}

func TestOpencodeInstallPlugin_NoSource(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, _ := reg.Lookup("opencode")
	a := agentinstall.Artifact{Kind: agentinstall.KindPlugin, Name: "demo",
		Plugin: &agentinstall.PluginPayload{Plugin: "p", Marketplace: "mp"}}
	r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home})
	if r.Status != agentinstall.StatusSkipped || r.Capability != agentinstall.CapabilityUnsupported {
		t.Fatalf("got %+v", r)
	}
}

// ---- hermes / goose (seam-only, not this slice) ------------------------------

func TestHermesInstallPlugin_Skipped(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, _ := reg.Lookup("hermes")
	a := agentinstall.Artifact{Kind: agentinstall.KindPlugin, Name: "demo",
		Plugin: &agentinstall.PluginPayload{Plugin: "p", Marketplace: "mp"}}
	r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home})
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("hermes plugin should be skipped, got %+v", r)
	}
}

func TestGooseInstallPlugin_Skipped(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, _ := reg.Lookup("goose")
	a := agentinstall.Artifact{Kind: agentinstall.KindPlugin, Name: "demo",
		Plugin: &agentinstall.PluginPayload{Plugin: "p", Marketplace: "mp"}}
	r := e.InstallPlugin(a, agentinstall.Options{HomeDir: home})
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("goose plugin should be skipped, got %+v", r)
	}
}
