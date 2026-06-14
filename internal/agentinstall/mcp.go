package agentinstall

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

// ---- shared helpers -------------------------------------------------------

// validateMCPName rejects names that are not safe single path segments.
// It reuses the same rules as validateSkillName.
func validateMCPName(name string) error {
	if name == "" {
		return fmt.Errorf("mcp name must not be empty")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("mcp name must not contain path separators: %q", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("mcp name must not be %q", name)
	}
	if filepath.Clean(name) != name {
		return fmt.Errorf("mcp name is not a clean single path segment: %q", name)
	}
	return nil
}

// resolveStdioEnv returns the merged env map for a stdio MCP artifact:
// base env from a.MCP.Stdio.Env, overridden/appended by secret file contents.
// Each secret ref must be a clean single segment; its value is read from opts.SecretsDir.
func resolveStdioEnv(a Artifact, opts Options) (map[string]string, error) {
	out := make(map[string]string)
	if a.MCP != nil && a.MCP.Stdio != nil {
		for k, v := range a.MCP.Stdio.Env {
			out[k] = v
		}
	}
	if a.MCP == nil {
		return out, nil
	}
	for _, ref := range a.MCP.SecretRefs {
		if err := validateMCPName(ref); err != nil {
			return nil, fmt.Errorf("invalid secret ref %q: %w", ref, err)
		}
		b, err := os.ReadFile(filepath.Join(opts.SecretsDir, ref))
		if err != nil {
			return nil, fmt.Errorf("read secret %q: %w", ref, err)
		}
		out[ref] = strings.TrimRight(string(b), "\n")
	}
	return out, nil
}

// hasSecrets reports whether the artifact carries any sensitive values that
// should force the config file to 0600:
//   - SecretRefs: resolved from files in SecretsDir
//   - Sensitive: caller-flagged
//   - HTTP with Headers: headers commonly carry Authorization tokens
func hasSecrets(a Artifact) bool {
	if a.MCP == nil {
		return false
	}
	return len(a.MCP.SecretRefs) > 0 || a.Sensitive || (a.MCP.HTTP != nil && len(a.MCP.HTTP.Headers) > 0)
}

// filePerm determines the file permission to use.
//   - if secret → always 0600
//   - else if existing file → preserve its mode
//   - else → defaultPerm
func filePerm(path string, secret bool, defaultPerm fs.FileMode) fs.FileMode {
	if secret {
		return 0o600
	}
	info, err := os.Stat(path)
	if err == nil {
		return info.Mode().Perm()
	}
	return defaultPerm
}

// atomicWriteFile writes data to path atomically (temp file + rename).
// Parent directories are created with mode 0755.
// The file is written with perm (chmod applied before close, before rename).
func atomicWriteFile(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-mcp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file to %s: %w", path, err)
	}
	ok = true
	return nil
}

// readJSONMap reads path into a map[string]interface{}; returns empty map if absent.
func readJSONMap(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]interface{}), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m == nil {
		m = make(map[string]interface{})
	}
	return m, nil
}

// upsertStringMap ensures root[key] is a map[string]interface{}, creating it if needed.
func upsertStringMap(root map[string]interface{}, key string) map[string]interface{} {
	if v, ok := root[key].(map[string]interface{}); ok {
		return v
	}
	m := make(map[string]interface{})
	root[key] = m
	return m
}

// ---- claudeEmitter --------------------------------------------------------

// InstallMCP installs an MCP server into ~/.claude.json (USER scope).
// stdio: {"command","args"(omit if empty),"env"(omit if empty)}
// http:  {"type":"http","url","headers"(omit if empty)}
func (e claudeEmitter) InstallMCP(a Artifact, opts Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindMCP, Name: a.Name}

	if a.MCP == nil {
		base.Status = StatusSkipped
		base.Reason = "mcp artifact has no MCP payload"
		return base
	}
	if err := validateMCPName(a.Name); err != nil {
		base.Status = StatusSkipped
		base.Reason = err.Error()
		return base
	}

	path := e.layout.MCPPath
	root, err := readJSONMap(path)
	if err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	var server map[string]interface{}
	switch {
	case a.MCP.Stdio != nil:
		env, err := resolveStdioEnv(a, opts)
		if err != nil {
			base.Status = StatusFailed
			base.Reason = err.Error()
			return base
		}
		s := map[string]interface{}{"command": a.MCP.Stdio.Command}
		if len(a.MCP.Stdio.Args) > 0 {
			s["args"] = stringSliceToInterface(a.MCP.Stdio.Args)
		}
		if len(env) > 0 {
			s["env"] = stringMapToInterface(env)
		}
		server = s
	case a.MCP.HTTP != nil:
		s := map[string]interface{}{"type": "http", "url": a.MCP.HTTP.URL}
		if len(a.MCP.HTTP.Headers) > 0 {
			s["headers"] = stringMapToInterface(a.MCP.HTTP.Headers)
		}
		server = s
	default:
		base.Status = StatusSkipped
		base.Reason = "mcp artifact has no transport"
		return base
	}

	mcpServers := upsertStringMap(root, "mcpServers")
	mcpServers[a.Name] = server

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("marshal .claude.json: %v", err)
		return base
	}

	// ~/.claude.json holds OAuth credentials as well as MCP secrets; always 0600.
	perm := fs.FileMode(0o600)
	if err := atomicWriteFile(path, data, perm); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	base.Status = StatusApplied
	return base
}

// ---- codexEmitter ---------------------------------------------------------

// InstallMCP installs an MCP server into $CODEX_HOME/config.toml (USER scope).
// stdio: command, args (omit empty), env (omit empty)
// http:  url, http_headers (omit empty)
func (e codexEmitter) InstallMCP(a Artifact, opts Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindMCP, Name: a.Name}

	if a.MCP == nil {
		base.Status = StatusSkipped
		base.Reason = "mcp artifact has no MCP payload"
		return base
	}
	if err := validateMCPName(a.Name); err != nil {
		base.Status = StatusSkipped
		base.Reason = err.Error()
		return base
	}

	path := e.layout.MCPPath

	// Read existing TOML or start fresh.
	var doc map[string]interface{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			doc = make(map[string]interface{})
		} else {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("read %s: %v", path, err)
			return base
		}
	} else {
		if err := toml.Unmarshal(data, &doc); err != nil {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("parse %s: %v", path, err)
			return base
		}
		if doc == nil {
			doc = make(map[string]interface{})
		}
	}

	var server map[string]interface{}
	switch {
	case a.MCP.Stdio != nil:
		env, err := resolveStdioEnv(a, opts)
		if err != nil {
			base.Status = StatusFailed
			base.Reason = err.Error()
			return base
		}
		s := map[string]interface{}{"command": a.MCP.Stdio.Command}
		if len(a.MCP.Stdio.Args) > 0 {
			s["args"] = stringSliceToInterface(a.MCP.Stdio.Args)
		}
		if len(env) > 0 {
			s["env"] = stringMapToInterface(env)
		}
		server = s
	case a.MCP.HTTP != nil:
		s := map[string]interface{}{"url": a.MCP.HTTP.URL}
		if len(a.MCP.HTTP.Headers) > 0 {
			s["http_headers"] = stringMapToInterface(a.MCP.HTTP.Headers)
		}
		server = s
	default:
		base.Status = StatusSkipped
		base.Reason = "mcp artifact has no transport"
		return base
	}

	mcpServers := upsertStringMap(doc, "mcp_servers")
	mcpServers[a.Name] = server

	out, err := toml.Marshal(doc)
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("marshal %s: %v", path, err)
		return base
	}

	perm := filePerm(path, hasSecrets(a), 0o644)
	if err := atomicWriteFile(path, out, perm); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	base.Status = StatusApplied
	return base
}

// ---- opencodeEmitter ------------------------------------------------------

// jsonPointerEscape escapes a token for use in a JSON Pointer (RFC 6901).
// '~' is encoded as '~0'; '/' is encoded as '~1'.
func jsonPointerEscape(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// InstallMCP installs an MCP server into ~/.config/opencode/opencode.json (USER scope).
// stdio (type "local"):  type, enabled:true, command (single array = cmd+args), environment (omit empty)
// http  (type "remote"): type, enabled:true, url, headers (omit empty)
// Format-preserving update via hujson RFC-6902 patch (preserves existing comments).
func (e opencodeEmitter) InstallMCP(a Artifact, opts Options) Report {
	base := Report{Agent: e.layout.Name, Kind: KindMCP, Name: a.Name}

	if a.MCP == nil {
		base.Status = StatusSkipped
		base.Reason = "mcp artifact has no MCP payload"
		return base
	}
	if err := validateMCPName(a.Name); err != nil {
		base.Status = StatusSkipped
		base.Reason = err.Error()
		return base
	}

	path := e.layout.MCPPath

	// Read existing file or start from empty object.
	var raw []byte
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			raw = []byte("{}")
		} else {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("read %s: %v", path, err)
			return base
		}
	} else {
		raw = data
	}

	// Parse with hujson (JSONC-aware).
	v, err := hujson.Parse(raw)
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("parse JSONC %s: %v", path, err)
		return base
	}

	// Build server object.
	var server map[string]interface{}
	switch {
	case a.MCP.Stdio != nil:
		env, err := resolveStdioEnv(a, opts)
		if err != nil {
			base.Status = StatusFailed
			base.Reason = err.Error()
			return base
		}
		cmd := append([]string{a.MCP.Stdio.Command}, a.MCP.Stdio.Args...)
		s := map[string]interface{}{
			"type":    "local",
			"enabled": true,
			"command": stringSliceToInterface(cmd),
		}
		if len(env) > 0 {
			s["environment"] = stringMapToInterface(env)
		}
		server = s
	case a.MCP.HTTP != nil:
		s := map[string]interface{}{
			"type":    "remote",
			"enabled": true,
			"url":     a.MCP.HTTP.URL,
		}
		if len(a.MCP.HTTP.Headers) > 0 {
			s["headers"] = stringMapToInterface(a.MCP.HTTP.Headers)
		}
		server = s
	default:
		base.Status = StatusSkipped
		base.Reason = "mcp artifact has no transport"
		return base
	}

	serverJSON, err := json.Marshal(server)
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("marshal server object: %v", err)
		return base
	}

	// Ensure /mcp exists by checking the standardized copy.
	stdCopy, err := hujson.Standardize(v.Pack())
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("standardize for check: %v", err)
		return base
	}
	var rootCheck map[string]interface{}
	if err := json.Unmarshal(stdCopy, &rootCheck); err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("unmarshal for check: %v", err)
		return base
	}
	if _, hasMCP := rootCheck["mcp"]; !hasMCP {
		addMCP := `[{"op":"add","path":"/mcp","value":{}}]`
		if err := v.Patch([]byte(addMCP)); err != nil {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("patch add /mcp: %v", err)
			return base
		}
	}

	// Upsert the server entry (RFC-6902 "add" replaces existing members).
	ptr := "/mcp/" + jsonPointerEscape(a.Name)
	patch := fmt.Sprintf(`[{"op":"add","path":%q,"value":%s}]`, ptr, serverJSON)
	if err := v.Patch([]byte(patch)); err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("patch add %s: %v", ptr, err)
		return base
	}

	out := v.Pack()
	perm := filePerm(path, hasSecrets(a), 0o644)
	if err := atomicWriteFile(path, out, perm); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	base.Status = StatusApplied
	return base
}

// ---- gooseEmitter ---------------------------------------------------------

// InstallMCP is permanently skipped for goose (deferred).
func (e gooseEmitter) InstallMCP(a Artifact, _ Options) Report {
	return Report{
		Agent:  e.layout.Name,
		Kind:   KindMCP,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: "goose MCP deferred",
	}
}

// ---- type-conversion helpers ----------------------------------------------

func stringSliceToInterface(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func stringMapToInterface(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
