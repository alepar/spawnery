// Package skillfetch fetches a GitHub repo tarball, validates a top-level SKILL.md,
// canonically repacks the content, and produces a zstd-compressed deterministic tar.
//
// Security surface: this package is the CP's first arbitrary-URL egress. It enforces:
//   - Per-hop host allowlist (github.com, api.github.com, codeload.github.com)
//   - Resolved-IP blocking for loopback/RFC1918/link-local/CGNAT/metadata ranges
//   - Streaming size caps (wire and decompressed) before any buffering
//   - Tar-entry safety (no symlinks, hardlinks, devices, absolute paths, .. escapes)
//   - Canonical deterministic repack for stable sha256 content identity
package skillfetch

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"gopkg.in/yaml.v3"
)

const (
	// WireCapBytes is the maximum compressed size of the tarball over the wire (~20 MiB).
	WireCapBytes = 20 * 1024 * 1024
	// DecompressedCapBytes is the maximum decompressed size before tar parsing (~50 MiB).
	DecompressedCapBytes = 50 * 1024 * 1024
	// PlainTarCapBytes is the maximum plain-tar size after repack (~50 MiB).
	PlainTarCapBytes = 50 * 1024 * 1024
	// FileCountCap is the maximum number of entries in the tarball.
	FileCountCap = 10_000
	// HTTPTimeout is the per-fetch HTTP deadline.
	HTTPTimeout = 60 * time.Second
)

// RepoRef is the parsed, normalized GitHub repo reference.
type RepoRef struct {
	Owner string
	Repo  string
}

// Result is the output of a successful Fetch call.
type Result struct {
	Owner           string
	Repo            string
	Name            string // sanitized catalog name (from frontmatter or request)
	Description     string
	NameWarning     string // non-empty when request name and frontmatter name differ
	PlainTarSHA256  string // hex sha256 of the plain (uncompressed) canonical tar
	CompressedBytes []byte // zstd-compressed canonical tar
	PlainSize       int64  // plain tar size in bytes
}

// Fetcher fetches a GitHub skill and returns a Result.
type Fetcher interface {
	Fetch(ctx context.Context, ref RepoRef, gitRef, subdir, requestedName, description string) (Result, error)
}

// Config holds the runtime parameters for the fetcher.
type Config struct {
	// GitHubToken is an optional Bearer token for authenticated GitHub API access.
	// Raises the shared rate-limit from ~60/hr to 5000/hr per source IP.
	GitHubToken string
	// ZstdLevel is the zstd compression level (1–19; default ~3 if 0).
	ZstdLevel int
}

// New returns a Fetcher with the given config.
func New(cfg Config) Fetcher {
	if cfg.ZstdLevel == 0 {
		cfg.ZstdLevel = 3
	}
	return &fetcher{cfg: cfg, client: newSecureClient()}
}

type fetcher struct {
	cfg    Config
	client *secureClient
}

// ParseRepoURL parses a raw input (owner/repo or https://github.com/owner/repo) into a RepoRef.
// It strips .git, trailing slash, ?query, #fragment.
// It rejects /tree/... and /blob/... deep paths with an actionable error.
func ParseRepoURL(raw string) (RepoRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return RepoRef{}, fmt.Errorf("URL is required")
	}

	var owner, repo string

	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		u, err := url.Parse(raw)
		if err != nil {
			return RepoRef{}, fmt.Errorf("invalid URL %q: %w", raw, err)
		}
		if u.Hostname() != "github.com" {
			return RepoRef{}, fmt.Errorf("only github.com URLs are supported, got %q", u.Hostname())
		}
		p := strings.TrimPrefix(u.Path, "/")
		p = strings.TrimSuffix(p, "/")
		parts := strings.SplitN(p, "/", 3)
		if len(parts) < 2 {
			return RepoRef{}, fmt.Errorf("URL %q must be of the form https://github.com/owner/repo", raw)
		}
		// Reject deep paths (/tree/... or /blob/...)
		if len(parts) >= 3 && (parts[2] == "tree" || strings.HasPrefix(parts[2], "tree/") ||
			parts[2] == "blob" || strings.HasPrefix(parts[2], "blob/")) {
			return RepoRef{}, fmt.Errorf("deep GitHub URL (tree/blob path) is ambiguous; paste the repo URL and set ref/subdir explicitly: %q", raw)
		}
		if len(parts) >= 3 && parts[2] != "" {
			return RepoRef{}, fmt.Errorf("unexpected path segment %q in URL %q; use https://github.com/owner/repo and pass ref/subdir separately", parts[2], raw)
		}
		owner = parts[0]
		repo = strings.TrimSuffix(parts[1], ".git")
	} else {
		// owner/repo shorthand
		raw = strings.TrimSuffix(raw, "/")
		parts := strings.SplitN(raw, "/", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return RepoRef{}, fmt.Errorf("input %q must be owner/repo or https://github.com/owner/repo", raw)
		}
		if len(parts) > 2 {
			return RepoRef{}, fmt.Errorf("input %q has extra path segments; use owner/repo and set subdir separately", raw)
		}
		owner = parts[0]
		repo = strings.TrimSuffix(parts[1], ".git")
	}

	if owner == "" || repo == "" {
		return RepoRef{}, fmt.Errorf("could not parse owner/repo from %q", raw)
	}
	return RepoRef{Owner: owner, Repo: repo}, nil
}

// tarballURL returns the GitHub API tarball URL for the given owner/repo/ref.
// ref may be empty (default branch).
func tarballURL(owner, repo, ref string) string {
	base := fmt.Sprintf("https://api.github.com/repos/%s/%s/tarball", owner, repo)
	if ref == "" {
		return base
	}
	return base + "/" + ref
}

// skillFrontmatter holds the parsed SKILL.md YAML front matter.
type skillFrontmatter struct {
	Name string `yaml:"name"`
}

// parseSkillMD parses the YAML front matter from SKILL.md content.
// Returns empty struct when there is no front matter (not an error).
func parseSkillMD(content []byte) (skillFrontmatter, error) {
	s := string(content)
	if !strings.HasPrefix(s, "---") {
		return skillFrontmatter{}, nil
	}
	// Find closing ---
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return skillFrontmatter{}, nil
	}
	yamlPart := rest[:end]
	var fm skillFrontmatter
	_ = yaml.Unmarshal([]byte(yamlPart), &fm) // garbled frontmatter is not a hard failure; name will come from request
	return fm, nil
}

// validateName checks that name is a clean single path segment (no slashes, dots-only, etc.).
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required (no SKILL.md frontmatter name and no request name)")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("name %q must be a single path segment (no slashes)", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("name %q is not a valid path segment", name)
	}
	return nil
}

// sanitizeName converts a repo name (or frontmatter name) into a clean single path segment:
// lowercase, spaces/underscores to hyphens, strip leading/trailing hyphens.
func sanitizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.NewReplacer(" ", "-", "_", "-").Replace(name)
	name = strings.Trim(name, "-.")
	return name
}

// tarEntry is a normalized file entry for the canonical repack.
type tarEntry struct {
	path    string
	mode    int64
	isDir   bool
	content []byte
}

// Fetch downloads the GitHub tarball, validates, canonically repacks, and returns the Result.
func (f *fetcher) Fetch(ctx context.Context, ref RepoRef, gitRef, subdir, requestedName, description string) (Result, error) {
	rawURL := tarballURL(ref.Owner, ref.Repo, gitRef)

	// Download and unpack into in-memory entries
	entries, err := f.client.fetchAndUnpack(ctx, rawURL, f.cfg.GitHubToken, subdir)
	if err != nil {
		return Result{}, err
	}

	// Require SKILL.md at the top level (after wrapper strip + subdir descent)
	var skillMDContent []byte
	skillMDName := "SKILL.md"
	for _, e := range entries {
		if e.path == skillMDName {
			skillMDContent = e.content
			break
		}
	}
	if skillMDContent == nil {
		if subdir != "" {
			return Result{}, fmt.Errorf("no SKILL.md found at %q", subdir)
		}
		return Result{}, fmt.Errorf("no SKILL.md found in repository root")
	}

	// Parse frontmatter
	fm, _ := parseSkillMD(skillMDContent)

	// Reconcile name
	resolvedName := requestedName
	nameWarning := ""
	if resolvedName == "" {
		if fm.Name != "" {
			resolvedName = sanitizeName(fm.Name)
		} else {
			// Fall back to repo name (or subdir leaf)
			if subdir != "" {
				resolvedName = sanitizeName(path.Base(subdir))
			} else {
				resolvedName = sanitizeName(ref.Repo)
			}
		}
	} else {
		// Request name supplied; warn if it differs from frontmatter
		if fm.Name != "" {
			sanitized := sanitizeName(fm.Name)
			if sanitized != sanitizeName(resolvedName) {
				nameWarning = fmt.Sprintf("request name %q differs from SKILL.md frontmatter name %q", resolvedName, fm.Name)
			}
		}
		resolvedName = sanitizeName(resolvedName)
	}
	if err := validateName(resolvedName); err != nil {
		return Result{}, err
	}

	// Canonical repack
	plainTar, err := canonicalRepack(entries)
	if err != nil {
		return Result{}, fmt.Errorf("repack: %w", err)
	}
	if int64(len(plainTar)) > PlainTarCapBytes {
		return Result{}, fmt.Errorf("skill tar exceeds size cap (%d > %d bytes)", len(plainTar), PlainTarCapBytes)
	}

	// sha256 over the PLAIN tar
	h := sha256.Sum256(plainTar)
	sha256hex := hex.EncodeToString(h[:])

	// zstd-compress
	level := f.cfg.ZstdLevel
	if level <= 0 {
		level = 3
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(level)))
	if err != nil {
		return Result{}, fmt.Errorf("zstd init: %w", err)
	}
	compressed := enc.EncodeAll(plainTar, nil)

	return Result{
		Owner:           ref.Owner,
		Repo:            ref.Repo,
		Name:            resolvedName,
		Description:     description,
		NameWarning:     nameWarning,
		PlainTarSHA256:  sha256hex,
		CompressedBytes: compressed,
		PlainSize:       int64(len(plainTar)),
	}, nil
}

// canonicalRepack builds a deterministic USTAR tar from the entries:
//   - sorted by path
//   - mtime=0, uid/gid=0, uname/gname empty
//   - mode: files => 0644 (0755 if user-exec bit set); dirs => 0755
//   - no PAX/GNU variance
func canonicalRepack(entries []tarEntry) ([]byte, error) {
	// Sort entries by path for determinism
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].path < entries[j].path
	})

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		var mode int64
		if e.isDir {
			mode = 0o755
		} else {
			// Normalize: 0644, preserve user-exec bit
			if e.mode&0o100 != 0 {
				mode = 0o755
			} else {
				mode = 0o644
			}
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.path,
			Mode:     mode,
			Size:     int64(len(e.content)),
			// Zero mtime, uid/gid, uname/gname for canonical output
			ModTime: time.Time{},
			Uid:     0,
			Gid:     0,
			Uname:   "",
			Gname:   "",
			Format:  tar.FormatUSTAR,
		}
		if e.isDir {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
			hdr.Name = strings.TrimSuffix(e.path, "/") + "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if !e.isDir && len(e.content) > 0 {
			if _, err := tw.Write(e.content); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// safeRelPath validates and cleans a tar entry path for safe extraction.
// Returns error if the path is absolute, contains ".." escapes, or is otherwise unsafe.
func safeRelPath(p string) (string, error) {
	p = path.Clean(p)
	if path.IsAbs(p) {
		return "", fmt.Errorf("absolute path rejected: %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("path escape rejected: %q", p)
		}
	}
	return p, nil
}
