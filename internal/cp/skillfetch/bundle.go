package skillfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ErrNoSkills is returned (wrapped) by discoverSkills/assembleBundle/FetchBundle when the fetched
// tree contains no SKILL.md anywhere. The CP handler detects this via errors.Is to distinguish
// the layout-change branch (§4.5: a re-ingest that no longer discovers a member set) from a
// first-ever ingest of a skill-less repo — a string match on the error text would be fragile.
var ErrNoSkills = errors.New("no SKILL.md found anywhere in the fetched tree")

// BundleResult is the output of a successful FetchBundle call: every skill discovered in the
// fetched repo (or subdir), each independently repacked (§4.2).
type BundleResult struct {
	Owner        string
	Repo         string
	SourceCommit string // commit sha recovered from the GitHub wrapper dir; "" if unknown
	Members      []Member
	// SkippedEntries lists non-regular tar entries (symlink, hardlink, device, fifo) that were
	// skipped, not fetched, across the whole bundle. nil when nothing was skipped.
	SkippedEntries []string
	// Warnings lists non-fatal discovery findings — e.g. a member root that hides nested
	// SKILL.mds. nil when there is nothing to warn about.
	Warnings []string
}

// Member is one skill discovered and repacked from a bundle fetch.
type Member struct {
	// Dir is the member's directory, relative to the repo root (i.e. including any requested
	// subdir). "" for a member at the repo root.
	Dir             string
	Name            string // sanitized catalog name (from frontmatter or dir/repo fallback)
	Description     string // sanitized SKILL.md frontmatter description (§4.9)
	PlainTarSHA256  string // hex sha256 of the plain (uncompressed) canonical member tar
	CompressedBytes []byte // zstd-compressed canonical member tar
	PlainSize       int64  // plain member tar size in bytes
}

// skillRoot is a directory (repo-relative, post wrapper-strip and requested-subdir descent)
// containing a SKILL.md.
type skillRoot struct {
	dir string // "." for the tree root
}

// discoverSkills walks entries (already wrapper-stripped and subdir-descended) and finds every
// skill root — any directory containing a SKILL.md. A root is a *member* iff no other root is an
// ancestor of it (prune: a member's entire subtree belongs to it, no matter how many nested
// SKILL.mds it hides). Every member that hides nested SKILL.mds gets a warning listing them
// (§4.2) — including a root SKILL.md that hides nested skills elsewhere in the tree.
//
// Returns an error, not zero members, when the tree contains no SKILL.md at all.
func discoverSkills(entries []tarEntry) ([]skillRoot, []string, error) {
	var allRoots []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.isDir {
			continue
		}
		if path.Base(e.path) != "SKILL.md" {
			continue
		}
		dir := path.Dir(e.path)
		if !seen[dir] {
			seen[dir] = true
			allRoots = append(allRoots, dir)
		}
	}
	if len(allRoots) == 0 {
		return nil, nil, ErrNoSkills
	}
	sort.Strings(allRoots)

	var members []skillRoot
	for _, r := range allRoots {
		if !hasAncestorRoot(allRoots, r) {
			members = append(members, skillRoot{dir: r})
		}
	}

	var warnings []string
	for _, m := range members {
		var nested []string
		for _, r := range allRoots {
			if r != m.dir && isAncestorDir(m.dir, r) {
				nested = append(nested, r)
			}
		}
		if len(nested) == 0 {
			continue
		}
		sort.Strings(nested)
		label := m.dir
		if label == "." {
			label = "repository root"
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s: %d nested skill(s) hidden by this member (%s); ingesting %s only — set subdir to ingest a nested one",
			label, len(nested), strings.Join(nested, ", "), label))
	}

	return members, warnings, nil
}

// isAncestorDir reports whether dir a is a strict ancestor of dir b (both repo-relative, "."
// meaning the tree root). "." is an ancestor of every other dir.
func isAncestorDir(a, b string) bool {
	if a == b {
		return false
	}
	if a == "." {
		return true
	}
	return strings.HasPrefix(b, a+"/")
}

// hasAncestorRoot reports whether any root in roots (other than r itself) is an ancestor of r.
func hasAncestorRoot(roots []string, r string) bool {
	for _, other := range roots {
		if other != r && isAncestorDir(other, r) {
			return true
		}
	}
	return false
}

// selectSubtree mirrors fetchAndUnpack's subdir descent (fetch.go) exactly: drop the entry equal
// to dir, drop entries not prefixed dir+"/", trim that prefix off the rest. dir == "." is the
// identity (the whole tree is already the member).
//
// entries are assumed already safeRelPath-cleaned (fetchAndUnpack's contract), so no re-cleaning
// is needed here — this is what makes a member's repack byte-identical to a standalone
// subdir=<dir> ingest of the same directory (see bundle_test.go).
func selectSubtree(entries []tarEntry, dir string) []tarEntry {
	if dir == "." {
		return entries
	}
	prefix := dir + "/"
	var out []tarEntry
	for _, e := range entries {
		if e.path == dir {
			continue
		}
		if !strings.HasPrefix(e.path, prefix) {
			continue
		}
		out = append(out, tarEntry{
			path:    strings.TrimPrefix(e.path, prefix),
			mode:    e.mode,
			isDir:   e.isDir,
			content: e.content,
		})
	}
	return out
}

// joinMemberDir joins a requested subdir with a member's repo-relative-to-subdir dir, returning
// "" for the repo root (rather than path.Join's ".").
func joinMemberDir(subdir, dir string) string {
	d := path.Join(subdir, dir)
	if d == "." {
		return ""
	}
	return d
}

// FetchBundle downloads the GitHub tarball, discovers every skill in it (pruning at nested skill
// roots), and independently canonically repacks each one. See discoverSkills and selectSubtree.
//
// Fails loud, before any member is returned, on: zero skills found; duplicate member names;
// duplicate member content (byte-identical member dirs); a member's plain tar exceeding
// PlainTarCapBytes.
func (f *fetcher) FetchBundle(ctx context.Context, ref RepoRef, gitRef, subdir string) (BundleResult, error) {
	rawURL := tarballURL(ref.Owner, ref.Repo, gitRef)

	unpacked, err := f.client.fetchAndUnpack(ctx, rawURL, f.cfg.GitHubToken, subdir)
	if err != nil {
		return BundleResult{}, err
	}
	return f.assembleBundle(ref, subdir, unpacked)
}

// assembleBundle is FetchBundle's discovery/repack/fail-loud logic, split out from the network
// fetch so it is directly testable against a fixture unpackResult — tarballURL always originates
// against api.github.com (fetch.go), so FetchBundle itself cannot be pointed at an httptest
// server; bundle_test.go instead calls secureClient.fetchAndUnpack against a fixture server, then
// assembleBundle, mirroring the pattern already used throughout skillfetch_test.go.
func (f *fetcher) assembleBundle(ref RepoRef, subdir string, unpacked unpackResult) (BundleResult, error) {
	var skippedEntries []string
	for _, se := range unpacked.skipped {
		skippedEntries = append(skippedEntries, fmt.Sprintf("%s (%s)", se.path, se.kind))
	}

	roots, warnings, err := discoverSkills(unpacked.entries)
	if err != nil {
		return BundleResult{}, err
	}

	level := f.cfg.ZstdLevel
	if level <= 0 {
		level = 3
	}

	names := map[string]string{} // sanitized name -> member dir (dup-name detection)
	shas := map[string]string{}  // plain tar sha256 -> member dir (dup-content detection)
	members := make([]Member, 0, len(roots))

	for _, root := range roots {
		subEntries := selectSubtree(unpacked.entries, root.dir)

		var skillMDContent []byte
		for _, e := range subEntries {
			if e.path == "SKILL.md" {
				skillMDContent = e.content
				break
			}
		}
		if skillMDContent == nil {
			// discoverSkills found SKILL.md at this exact dir; selectSubtree mirrors the same
			// prefix logic, so this would only happen on an internal inconsistency.
			return BundleResult{}, fmt.Errorf("internal error: SKILL.md not found in selected subtree %q", root.dir)
		}

		fm, _ := parseSkillMD(skillMDContent)

		memberDir := joinMemberDir(subdir, root.dir)

		name := sanitizeName(fm.Name)
		if name == "" {
			if memberDir != "" {
				name = sanitizeName(path.Base(memberDir))
			} else {
				name = sanitizeName(ref.Repo)
			}
		}
		if err := validateName(name); err != nil {
			return BundleResult{}, fmt.Errorf("member %q: %w", memberDirLabel(memberDir), err)
		}

		description := sanitizeDescription(fm.Description)

		plainTar, err := canonicalRepack(subEntries)
		if err != nil {
			return BundleResult{}, fmt.Errorf("repack member %q: %w", memberDirLabel(memberDir), err)
		}
		if err := plainTarCapErr(int64(len(plainTar)), f.cfg.PlainTarCapBytes); err != nil {
			return BundleResult{}, fmt.Errorf("member %q: %w", memberDirLabel(memberDir), err)
		}

		h := sha256.Sum256(plainTar)
		shaHex := hex.EncodeToString(h[:])

		if prevDir, ok := names[name]; ok {
			return BundleResult{}, fmt.Errorf(
				"duplicate member name %q: %q and %q both resolve to it; rename one in its SKILL.md frontmatter or ingest it separately via subdir",
				name, memberDirLabel(prevDir), memberDirLabel(memberDir))
		}
		names[name] = memberDir

		if prevDir, ok := shas[shaHex]; ok {
			return BundleResult{}, fmt.Errorf(
				"duplicate member content: %q and %q are byte-identical (sha256 %s)",
				memberDirLabel(prevDir), memberDirLabel(memberDir), shaHex)
		}
		shas[shaHex] = memberDir

		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(level)))
		if err != nil {
			return BundleResult{}, fmt.Errorf("zstd init: %w", err)
		}
		compressed := enc.EncodeAll(plainTar, nil)

		members = append(members, Member{
			Dir:             memberDir,
			Name:            name,
			Description:     description,
			PlainTarSHA256:  shaHex,
			CompressedBytes: compressed,
			PlainSize:       int64(len(plainTar)),
		})
	}

	return BundleResult{
		Owner:          ref.Owner,
		Repo:           ref.Repo,
		SourceCommit:   unpacked.sourceCommit,
		Members:        members,
		SkippedEntries: skippedEntries,
		Warnings:       warnings,
	}, nil
}

// memberDirLabel renders a member dir for error messages, using "<repo root>" for the "" case.
func memberDirLabel(dir string) string {
	if dir == "" {
		return "<repo root>"
	}
	return dir
}
