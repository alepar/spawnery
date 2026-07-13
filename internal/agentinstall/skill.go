package agentinstall

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"spawnery/internal/agentinstall/spec"
)

// installSkill is the shared skill-install implementation used by every emitter.
//
// Each skill installs once into the canonical dir (~/.agents/skills/<name>/, shared
// by every agent) and, if layout.SkillPath is non-empty, a second REAL COPY (never a
// symlink — claude-code #31005/#38051/#25367) into the agent's native skill dir. Both
// copies are atomic upsert-by-name: copy the source tree into a sibling temp dir, stamp
// an ownership marker, then atomically replace the destination with os.Rename.
//
// Before either copy is touched, every destination is checked for a name-squat: a
// pre-existing directory occupying <name> that was not itself installed by a prior
// spawnery apply (see canonical.go's checkSquat). A squat on either destination fails
// the whole install — nothing is written, including the canonical copy.
func installSkill(layout AgentLayout, a Artifact, opts Options) Report {
	base := Report{
		Agent: layout.Name,
		Kind:  KindSkill,
		Name:  a.Name,
	}

	// Validate Skill payload is present.
	if a.Skill == nil {
		base.Status = StatusSkipped
		base.Reason = "skill artifact has no Skill payload"
		return base
	}

	// Path confinement: name must be a clean single segment.
	if err := validateSkillName(a.Name); err != nil {
		base.Status = StatusSkipped
		base.Reason = err.Error()
		return base
	}

	// Resolve source directory.
	src := a.Skill.Dir
	if !filepath.IsAbs(src) {
		src = filepath.Join(opts.ArtifactsDir, src)
	}

	// Verify source is a directory.
	srcInfo, err := os.Stat(src)
	if err != nil {
		base.Status = StatusFailed
		if os.IsNotExist(err) {
			base.Reason = fmt.Sprintf("skill source directory does not exist: %s", src)
		} else {
			base.Reason = fmt.Sprintf("skill source directory not accessible: %v", err)
		}
		return base
	}
	if !srcInfo.IsDir() {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("skill source path is not a directory: %s", src)
		return base
	}

	// Verify SKILL.md is present in source.
	skillMDPath := filepath.Join(src, "SKILL.md")
	if _, err := os.Stat(skillMDPath); err != nil {
		base.Status = StatusFailed
		if os.IsNotExist(err) {
			base.Reason = fmt.Sprintf("skill source directory missing SKILL.md: %s", src)
		} else {
			base.Reason = fmt.Sprintf("cannot access SKILL.md in source: %v", err)
		}
		return base
	}

	if opts.HomeDir == "" {
		base.Status = StatusFailed
		base.Reason = "HomeDir not set: cannot resolve canonical skills dir"
		return base
	}

	// Every skill installs once into the canonical dir; agents that also need a
	// real native copy (currently claude, plus codex's legacy compat copy) name it
	// via layout.SkillPath.
	dirs := []string{CanonicalSkillsDir(opts.HomeDir)}
	if layout.SkillPath != "" {
		dirs = append(dirs, layout.SkillPath)
	}

	dests := make([]string, len(dirs))
	for i, dir := range dirs {
		dests[i] = filepath.Join(dir, a.Name)
	}

	// Name-squat guard: runs before any destination is touched, so a squat on either
	// destination blocks the whole install.
	if err := checkSquat(dests); err != nil {
		base.Status = StatusFailed
		base.Reason = err.Error()
		return base
	}

	// Note which destinations were previously spawnery-managed, so a re-apply's report
	// records the overwrite (not just silently upserts).
	var overwritten []string
	for _, dest := range dests {
		if exists, managed, err := destOwnership(dest); err != nil {
			base.Status = StatusFailed
			base.Reason = err.Error()
			return base
		} else if exists && managed {
			overwritten = append(overwritten, dest)
		}
	}

	marker := skillMarker{
		Name:           a.Name,
		ProfileID:      opts.ProfileID,
		ProfileVersion: opts.ProfileVersion,
		InstalledBy:    "spawnery",
	}

	var skipped []string
	var sanitized bool
	for _, dir := range dirs {
		s, changed, err := installTreeAt(dir, a.Name, src, marker)
		if err != nil {
			base.Status = StatusFailed
			base.Reason = fmt.Sprintf("install skill to %s: %v", dir, err)
			return base
		}
		skipped = append(skipped, s...)
		sanitized = sanitized || changed
	}

	base.Status = StatusApplied
	var reasonParts []string
	if len(skipped) > 0 {
		reasonParts = append(reasonParts, fmt.Sprintf("skipped %d symlink(s): %s", len(skipped), strings.Join(skipped, ", ")))
	}
	if len(overwritten) > 0 {
		reasonParts = append(reasonParts, fmt.Sprintf("overwrote previously-managed skill install at: %s", strings.Join(overwritten, ", ")))
	}
	if sanitized {
		reasonParts = append(reasonParts, "sanitized SKILL.md frontmatter (description capped to 1 KiB)")
	}
	base.Reason = strings.Join(reasonParts, "; ")
	return base
}

// installTreeAt performs the atomic upsert-by-name of src into dir/name: copy into a
// sibling temp dir, sanitize the staged SKILL.md's frontmatter (never the source — the
// stored tar upstream must stay byte-identical, sp-mwco.1.11), stamp the ownership marker,
// then atomically replace dir/name with os.Rename. Returns the relative paths of any
// symlinks skipped during the copy, and whether SKILL.md's frontmatter was rewritten.
func installTreeAt(dir, name, src string, marker skillMarker) (skippedSymlinks []string, sanitized bool, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, fmt.Errorf("create skills directory: %w", err)
	}

	dest := filepath.Join(dir, name)

	tmp, err := os.MkdirTemp(dir, ".tmp-"+name+"-")
	if err != nil {
		return nil, false, fmt.Errorf("create temp directory for skill: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()

	skipped, err := copyTree(src, tmp)
	if err != nil {
		return nil, false, fmt.Errorf("copy skill tree: %w", err)
	}

	sanitized, err = sanitizeSkillMD(tmp, name)
	if err != nil {
		return nil, false, fmt.Errorf("sanitize SKILL.md: %w", err)
	}

	if err := writeMarker(tmp, marker); err != nil {
		return nil, false, fmt.Errorf("write skill ownership marker: %w", err)
	}

	if err := os.RemoveAll(dest); err != nil {
		return nil, false, fmt.Errorf("remove previous skill install: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return nil, false, fmt.Errorf("rename temp dir to dest: %w", err)
	}
	ok = true

	// Restrict the skill top-level directory to owner-only access (0700).
	if err := os.Chmod(dest, 0o700); err != nil {
		return nil, false, fmt.Errorf("chmod skill top dir: %w", err)
	}

	return skipped, sanitized, nil
}

// validateSkillName returns an error if name is not a clean single path segment, or exceeds
// spec.MaxSkillNameBytes (§4.9: the name is loaded into a harness's system prompt alongside
// the description, so it gets the same bound).
func validateSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name must not be empty")
	}
	if len(name) > spec.MaxSkillNameBytes {
		return fmt.Errorf("skill name exceeds %d bytes: %q", spec.MaxSkillNameBytes, name)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("skill name must not contain path separators: %q", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("skill name must not be %q", name)
	}
	if filepath.Clean(name) != name {
		return fmt.Errorf("skill name is not a clean single path segment: %q", name)
	}
	return nil
}

// copyTree copies the contents of srcDir into dstDir, preserving file permissions.
// Symlinks are skipped (not followed); their relative paths are returned in skippedSymlinks.
// Permissions are applied via explicit os.Chmod after each MkdirAll/file-write to defeat
// any restrictive umask set by the calling process.
func copyTree(srcDir, dstDir string) (skippedSymlinks []string, err error) {
	err = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		// Compute destination path.
		rel, relErr := filepath.Rel(srcDir, path)
		if relErr != nil {
			return relErr
		}
		dst := filepath.Join(dstDir, rel)

		if d.IsDir() {
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			perm := info.Mode().Perm()
			if mkErr := os.MkdirAll(dst, perm); mkErr != nil {
				return mkErr
			}
			// Explicit chmod defeats any restrictive umask.
			return os.Chmod(dst, perm)
		}

		// Skip symlinks; record the relative path for the caller to report.
		if d.Type()&fs.ModeSymlink != 0 {
			skippedSymlinks = append(skippedSymlinks, rel)
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		return copyFile(path, dst, info.Mode().Perm())
	})
	return
}

// copyFile copies a single regular file from src to dst with the given permissions.
// An explicit os.Chmod is applied after close to defeat any restrictive umask.
func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source file %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create dest file %s: %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}

	if err := out.Close(); err != nil {
		return err
	}
	// Explicit chmod defeats any restrictive umask.
	return os.Chmod(dst, perm)
}
