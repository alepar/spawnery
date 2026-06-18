package main

import (
	"fmt"
	"strings"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/storage"
)

// parseMountFlag parses a single --mount value of the form
//
//	name=backend_uri[,opt...]
//
// into a cp.v1 MountBinding. The only recognised option is "create", which sets
// create_if_missing. The canonical github form is:
//
//	repo=github:owner/repo[,create]
//
// The client never names a credential: for a github mount slot the control plane
// derives the gh:<owner> link-ref at CreateSpawn (D1/T3) and rejects any client-
// supplied gh: credential. This flag therefore exposes no credential field at all.
func parseMountFlag(spec string) (*cpv1.MountBinding, error) {
	name, rest, ok := strings.Cut(spec, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return nil, fmt.Errorf("invalid --mount %q: want name=backend_uri[,create]", spec)
	}
	parts := strings.Split(rest, ",")
	backendURI := strings.TrimSpace(parts[0])
	if backendURI == "" {
		return nil, fmt.Errorf("invalid --mount %q: empty backend uri (want name=backend_uri[,create])", spec)
	}
	mb := &cpv1.MountBinding{Name: name, BackendUri: backendURI}
	for _, opt := range parts[1:] {
		switch strings.TrimSpace(opt) {
		case "":
			// tolerate a trailing comma
		case "create":
			mb.CreateIfMissing = true
		default:
			return nil, fmt.Errorf("invalid --mount %q: unknown option %q (only 'create' is supported)", spec, opt)
		}
	}
	// Validate the github form up front using the same parser the CP slot resolver
	// uses, so a typo fails fast at the client with a clear message. Non-github
	// backends are validated control-plane-side against the app's declared mounts.
	if strings.HasPrefix(backendURI, "github:") {
		if _, err := storage.ParseGitHubURI(backendURI); err != nil {
			return nil, fmt.Errorf("invalid --mount %q: %w", spec, err)
		}
	}
	return mb, nil
}

// parseMountFlags parses the repeatable --mount values into MountBindings,
// rejecting duplicate mount names (the CP also rejects duplicates, but a clear
// client-side error is friendlier).
func parseMountFlags(specs []string) ([]*cpv1.MountBinding, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]*cpv1.MountBinding, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, s := range specs {
		mb, err := parseMountFlag(s)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[mb.Name]; dup {
			return nil, fmt.Errorf("duplicate --mount name %q", mb.Name)
		}
		seen[mb.Name] = struct{}{}
		out = append(out, mb)
	}
	return out, nil
}
