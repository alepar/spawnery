package agentinstall

import "os"

// Environ is an abstraction over environment variable lookups,
// used to make detection logic hermetic in tests.
type Environ interface {
	// Home returns the effective home directory.
	Home() string
	// CodexHome returns $CODEX_HOME, or "" if unset.
	CodexHome() string
	// XDGConfigHome returns the XDG config home directory.
	// If $XDG_CONFIG_HOME is unset, defaults to $HOME/.config.
	XDGConfigHome() string
}

// OSEnviron is an Environ that reads from the real OS environment.
type OSEnviron struct{}

func (OSEnviron) Home() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return ""
}

func (OSEnviron) CodexHome() string {
	return os.Getenv("CODEX_HOME")
}

func (o OSEnviron) XDGConfigHome() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	return o.Home() + "/.config"
}

// MapEnviron implements Environ from a static map (for tests).
type MapEnviron map[string]string

func (m MapEnviron) Home() string {
	return m["HOME"]
}

func (m MapEnviron) CodexHome() string {
	return m["CODEX_HOME"]
}

func (m MapEnviron) XDGConfigHome() string {
	if x := m["XDG_CONFIG_HOME"]; x != "" {
		return x
	}
	if h := m["HOME"]; h != "" {
		return h + "/.config"
	}
	return ""
}

// Detect returns the list of agent names whose config root directories exist on disk.
// It uses the provided Environ to resolve paths (for test hermeticity).
// The returned slice is in canonical order: claude, codex, opencode, hermes, goose.
func Detect(env Environ) []string {
	reg := NewRegistry(env)
	layouts := reg.Layouts()

	var detected []string
	for _, layout := range layouts {
		if layout.ConfigRoot == "" {
			continue
		}
		if _, err := os.Stat(layout.ConfigRoot); err == nil {
			detected = append(detected, layout.Name)
		}
	}
	return detected
}
