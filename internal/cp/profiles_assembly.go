package cp

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/agentinstall"
	"spawnery/internal/agentinstall/spec"
	"spawnery/internal/cp/store"
)

// resolvedEntry pairs a profile entry with its resolved content bytes.
type resolvedEntry struct {
	entry   store.ProfileEntry
	content []byte
}

// assembleProfileArtifacts resolves a profile's non-secret entries into wire ArtifactSpecs:
// a manifest.json BYTES artifact + one TAR payload artifact per skill. Sensitive/secret
// artifacts are OUT of scope (sp-nrzf.4). Returns (nil, nil) when the profile has no
// non-secret entries (secrets-only / empty profile => no manifest, which is valid).
func (s *Server) assembleProfileArtifacts(ctx context.Context, _ store.Profile, entries []store.ProfileEntry) ([]*cpv1.ArtifactSpec, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	// Build target names from the registered agents.
	reg := agentinstall.NewRegistry(agentinstall.MapEnviron{})
	targetNames := make(map[string]bool)
	for _, name := range reg.Names() {
		targetNames[name] = true
	}

	// Resolve content per entry.
	items := make([]resolvedEntry, 0, len(entries))
	for _, entry := range entries {
		var content []byte
		switch entry.SourceKind {
		case store.ProfileSourceCatalog:
			ce, err := s.st.CustomizationCatalog().Get(ctx, entry.CatalogID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil, connect.NewError(connect.CodeInvalidArgument,
						fmt.Errorf("entry %q: unknown catalog ref %q", entry.Name, entry.CatalogID))
				}
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			content = ce.Content
		case store.ProfileSourceCustom:
			content = entry.CustomInline
		default:
			return nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("entry %q: unknown source kind %q", entry.Name, entry.SourceKind))
		}
		items = append(items, resolvedEntry{entry: entry, content: content})
	}

	manifest, payloadSpecs, err := buildManifestAndPayloads(items, targetNames)
	if err != nil {
		return nil, err
	}

	// Encode the manifest and prepend it as a BYTES ArtifactSpec.
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("marshal manifest: %w", err))
	}
	manifestSpec := &cpv1.ArtifactSpec{
		Id:              "manifest",
		Inline:          manifestJSON,
		ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
		DestPath:        "manifest.json",
		Mode:            0o644,
	}
	return append([]*cpv1.ArtifactSpec{manifestSpec}, payloadSpecs...), nil
}

// buildManifestAndPayloads turns resolved (entry, contentBytes) pairs into the canonical
// Manifest + TAR payload specs; targetNames is agentinstall.NewRegistry(env).Names().
// Returns the Manifest struct and only the skill TAR payload ArtifactSpecs (the caller
// is responsible for JSON-encoding the manifest and prepending it as a BYTES spec).
func buildManifestAndPayloads(items []resolvedEntry, targetNames map[string]bool) (spec.Manifest, []*cpv1.ArtifactSpec, error) {
	// Build the union of forbidden config keys across all agent layouts.
	reg := agentinstall.NewRegistry(agentinstall.MapEnviron{})
	forbiddenKeys := make(map[string]bool)
	for _, layout := range reg.Layouts() {
		for _, k := range layout.ForbiddenConfigKeys {
			forbiddenKeys[k] = true
		}
	}

	var artifacts []spec.Artifact
	var payloadSpecs []*cpv1.ArtifactSpec

	for _, item := range items {
		entry := item.entry
		content := item.content

		// Translate targets.
		targets, err := translateTargets(entry.Targets, targetNames)
		if err != nil {
			return spec.Manifest{}, nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("entry %q: %w", entry.Name, err))
		}

		a := spec.Artifact{
			Name:    entry.Name,
			Kind:    spec.Kind(entry.Kind),
			Targets: targets,
		}

		switch entry.Kind {
		case store.ProfileEntrySkill:
			if err := validateSkillTar(content, entry.Name); err != nil {
				return spec.Manifest{}, nil, err
			}
			payloadPath := "payloads/" + entry.EntryID
			a.Skill = &spec.SkillPayload{Dir: payloadPath}
			a.Payload = payloadPath
			payloadSpecs = append(payloadSpecs, &cpv1.ArtifactSpec{
				Id:              entry.EntryID,
				Inline:          content,
				ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR,
				TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
				DestPath:        payloadPath,
			})

		case store.ProfileEntryMCP:
			var payload spec.MCPPayload
			if err := json.Unmarshal(content, &payload); err != nil {
				return spec.Manifest{}, nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("entry %q: malformed MCP content: %w", entry.Name, err))
			}
			// Thread MCPSecretRefs from the entry metadata.
			payload.SecretRefs = entry.MCPSecretRefs
			a.MCP = &payload

		case store.ProfileEntryConfig:
			var payload spec.ConfigPayload
			if err := json.Unmarshal(content, &payload); err != nil {
				return spec.Manifest{}, nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("entry %q: malformed Config content: %w", entry.Name, err))
			}
			if err := validateConfigPayload(entry.Name, &payload, forbiddenKeys); err != nil {
				return spec.Manifest{}, nil, err
			}
			a.Config = &payload

		case store.ProfileEntryPlugin:
			var payload spec.PluginPayload
			if err := json.Unmarshal(content, &payload); err != nil {
				return spec.Manifest{}, nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("entry %q: malformed Plugin content: %w", entry.Name, err))
			}
			a.Plugin = &payload

		default:
			return spec.Manifest{}, nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("entry %q: unknown kind %q", entry.Name, entry.Kind))
		}

		artifacts = append(artifacts, a)
	}

	manifest := spec.Manifest{
		SchemaVersion: spec.CurrentSchemaVersion,
		Artifacts:     artifacts,
	}
	return manifest, payloadSpecs, nil
}

// translateTargets normalizes the entry's target list:
//   - empty list or list containing "all" → ["all-detected"]
//   - explicit list: each name must appear in targetNames (else error)
func translateTargets(targets []string, targetNames map[string]bool) ([]string, error) {
	if len(targets) == 0 {
		return []string{"all-detected"}, nil
	}
	for _, t := range targets {
		if t == "all" {
			return []string{"all-detected"}, nil
		}
	}
	// Validate each explicit name.
	for _, t := range targets {
		if !targetNames[t] {
			return nil, fmt.Errorf("unknown target agent %q", t)
		}
	}
	out := make([]string, len(targets))
	copy(out, targets)
	return out, nil
}

// validateSkillTar checks that content is a readable TAR archive containing a
// top-level SKILL.md entry. Returns a Connect CodeInvalidArgument error on failure.
func validateSkillTar(content []byte, entryName string) error {
	tr := tar.NewReader(bytes.NewReader(content))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("entry %q: unreadable skill TAR: %w", entryName, err))
		}
		// Normalize: strip "./" prefix and clean the path to match how spawnlet unpacks.
		name := path.Clean(hdr.Name)
		if name == "SKILL.md" {
			return nil
		}
	}
	return connect.NewError(connect.CodeInvalidArgument,
		fmt.Errorf("entry %q: skill TAR missing top-level SKILL.md", entryName))
}

// validateConfigPayload enforces defense-in-depth config key rules on the CP side:
//   - no forbidden key in normalized (those are launcher-reserved)
//   - no unknown key in normalized (catches typos + injection attempts)
//   - no forbidden key in any agent's native fragment
func validateConfigPayload(entryName string, cfg *spec.ConfigPayload, forbiddenKeys map[string]bool) error {
	for k := range cfg.Normalized {
		if forbiddenKeys[k] {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("entry %q: forbidden config key %q in normalized (launcher-reserved)", entryName, k))
		}
		if !agentinstall.ValidNormalizedConfigKeys[k] {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("entry %q: unknown normalized config key %q", entryName, k))
		}
	}
	// Check native passthrough for forbidden keys across all agents.
	for agentName, rawFragment := range cfg.Native {
		fragment, ok := rawFragment.(map[string]interface{})
		if !ok {
			continue // skip non-map values — engine will also skip them
		}
		for k := range fragment {
			if forbiddenKeys[k] {
				return connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("entry %q: forbidden config key %q in native[%q] (launcher-reserved)", entryName, k, agentName))
			}
		}
	}
	return nil
}
