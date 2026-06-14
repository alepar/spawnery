package agentinstall

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// installSkillTree is the shared skill-install implementation used by claude and codex.
// It performs an atomic upsert-by-name: copies the source tree into a temp dir under
// layout.SkillPath, then atomically replaces the destination with os.Rename.
func installSkillTree(layout AgentLayout, a Artifact, opts Options) Report {
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

	// Ensure the skills root exists.
	if err := os.MkdirAll(layout.SkillPath, 0o755); err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("create skills directory: %v", err)
		return base
	}

	dest := filepath.Join(layout.SkillPath, a.Name)

	// Atomic upsert: copy into a sibling temp dir, then rename into place.
	tmp, err := os.MkdirTemp(layout.SkillPath, ".tmp-"+a.Name+"-")
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("create temp directory for skill: %v", err)
		return base
	}
	// Clean up temp dir on failure.
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()

	skipped, err := copyTree(src, tmp)
	if err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("copy skill tree: %v", err)
		return base
	}

	// Remove previous install (if any), then rename tmp into place.
	if err := os.RemoveAll(dest); err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("remove previous skill install: %v", err)
		return base
	}
	if err := os.Rename(tmp, dest); err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("rename temp dir to dest: %v", err)
		return base
	}
	ok = true

	// Restrict the skill top-level directory to owner-only access (0700).
	if err := os.Chmod(dest, 0o700); err != nil {
		base.Status = StatusFailed
		base.Reason = fmt.Sprintf("chmod skill top dir: %v", err)
		return base
	}

	base.Status = StatusApplied
	if len(skipped) > 0 {
		base.Reason = fmt.Sprintf("skipped %d symlink(s): %s", len(skipped), strings.Join(skipped, ", "))
	}
	return base
}

// validateSkillName returns an error if name is not a clean single path segment.
func validateSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name must not be empty")
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
