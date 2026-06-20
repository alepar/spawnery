package config

import "fmt"

// validEnvs is the closed set of environments. SPAWNERY_ENV / --env must name one of these.
var validEnvs = map[string]bool{"dev": true, "staging": true, "prod": true}

// resolveEnv determines the active environment for the layered load. It is the first,
// bootstrap phase: a --env flag in args wins over the SPAWNERY_ENV environment variable, and
// the result is validated against the closed set {dev,staging,prod}.
//
// It is deliberately fail-closed: an unset or unknown value is an error, never a silent default
// to "dev" (which would boot a forgetful prod deployment on auth-relaxed config).
func resolveEnv(args []string, getenv func(string) (string, bool)) (string, error) {
	val, ok := envFromArgs(args)
	if !ok {
		val, ok = getenv("SPAWNERY_ENV")
	}
	if !ok {
		return "", fmt.Errorf("environment not set: set SPAWNERY_ENV (or pass --env) to one of dev|staging|prod")
	}
	if !validEnvs[val] {
		return "", fmt.Errorf("unknown environment %q: must be one of dev|staging|prod", val)
	}
	return val, nil
}

// envFromArgs scans CLI args for --env, supporting both --env=value and --env value forms.
func envFromArgs(args []string) (string, bool) {
	for i, a := range args {
		switch {
		case a == "--env":
			if i+1 < len(args) {
				return args[i+1], true
			}
		case len(a) > len("--env=") && a[:len("--env=")] == "--env=":
			return a[len("--env="):], true
		}
	}
	return "", false
}
