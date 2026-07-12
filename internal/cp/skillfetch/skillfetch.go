// Package skillfetch fetches a GitHub repo tarball, validates a top-level SKILL.md,
// canonically repacks the content, and produces a zstd-compressed deterministic tar.
//
// Security surface: this package is the CP's first arbitrary-URL egress. It enforces:
//   - Per-hop host allowlist (github.com, api.github.com, codeload.github.com)
//   - Resolved-IP blocking for loopback/RFC1918/link-local/CGNAT/metadata ranges
//   - Streaming size caps (wire and decompressed) before any buffering
//   - Tar-entry safety (non-regular entries skipped and reported; absolute paths and .. escapes rejected)
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
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"gopkg.in/yaml.v3"
)

const (
	// DefaultWireCapBytes is the default maximum compressed size of the tarball over the wire
	// (~20 MiB), used when Config.WireCapBytes is zero.
	DefaultWireCapBytes = 20 * 1024 * 1024
	// DefaultDecompressedCapBytes is the default maximum decompressed size before tar parsing
	// (~50 MiB), used when Config.DecompressedCapBytes is zero.
	DefaultDecompressedCapBytes = 50 * 1024 * 1024
	// DefaultPlainTarCapBytes is the default maximum plain-tar size after repack (~50 MiB),
	// used when Config.PlainTarCapBytes is zero.
	DefaultPlainTarCapBytes = 50 * 1024 * 1024
	// DefaultFileCountCap is the default maximum number of entries in the tarball, used when
	// Config.FileCountCap is zero.
	DefaultFileCountCap = 10_000
	// DefaultHTTPTimeout is the default per-fetch HTTP deadline, used when Config.HTTPTimeout
	// is zero.
	DefaultHTTPTimeout = 60 * time.Second
)

// plainTarCapErr returns an error if plainSize exceeds capBytes, else nil.
func plainTarCapErr(plainSize, capBytes int64) error {
	if plainSize > capBytes {
		return fmt.Errorf("skill tar exceeds size cap (%d > %d bytes)", plainSize, capBytes)
	}
	return nil
}

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
	// SkippedEntries lists non-regular tar entries (symlink, hardlink, device, fifo) that were
	// skipped, not fetched. Each element is formatted "<path> (<kind>)", e.g. "AGENTS.md (symlink)".
	// nil when nothing was skipped.
	SkippedEntries []string
	// SourceCommit is the commit sha recovered from the GitHub tarball wrapper dir
	// (owner-repo-<sha>/), or "" when the wrapper isn't GitHub-shaped (§4.9 commit pinning).
	SourceCommit string
}

// Fetcher fetches a GitHub skill and returns a Result.
type Fetcher interface {
	Fetch(ctx context.Context, ref RepoRef, gitRef, subdir, requestedName, description string) (Result, error)
}

// BundleFetcher fetches a GitHub repo tarball and discovers/repacks every skill in it (§4.2).
// Declared separately from Fetcher (rather than adding a method to it) so existing Fetcher
// implementations — notably internal/cp's fakeFetcher test double — are unaffected; *fetcher
// satisfies both.
type BundleFetcher interface {
	FetchBundle(ctx context.Context, ref RepoRef, gitRef, subdir string) (BundleResult, error)
}

// Config holds the runtime parameters for the fetcher.
//
// The caps here are enforced CP-side, at ingest. They are deliberately NOT the same knob as the
// host allowlist in fetch.go: allowedHosts is a code constant (see the comment there) because it
// only ever widens where a *redirect* may land, never where a fetch may originate — origination
// is pinned by ParseRepoURL + tarballURL. The caps below have no such origination hazard, so they
// are config-surfaced (skills.* in cmd/spawnery_cp/config.go).
type Config struct {
	// GitHubToken is an optional Bearer token for authenticated GitHub API access.
	// Raises the shared rate-limit from ~60/hr to 5000/hr per source IP.
	GitHubToken string
	// ZstdLevel is the zstd compression level (1–19; default ~3 if 0).
	ZstdLevel int
	// WireCapBytes is the maximum compressed size of the tarball over the wire.
	// Zero means DefaultWireCapBytes.
	WireCapBytes int64
	// DecompressedCapBytes is the maximum decompressed size before tar parsing.
	// Zero means DefaultDecompressedCapBytes.
	DecompressedCapBytes int64
	// PlainTarCapBytes is the maximum plain-tar size after canonical repack.
	// Zero means DefaultPlainTarCapBytes.
	PlainTarCapBytes int64
	// FileCountCap is the maximum number of entries in the tarball.
	// Zero means DefaultFileCountCap.
	FileCountCap int
	// HTTPTimeout is the per-fetch HTTP deadline. Zero means DefaultHTTPTimeout.
	HTTPTimeout time.Duration
}

// New returns a Fetcher with the given config. Zero-valued cap/timeout fields fall back to the
// package Default* constants.
func New(cfg Config) Fetcher {
	if cfg.ZstdLevel == 0 {
		cfg.ZstdLevel = 3
	}
	if cfg.WireCapBytes == 0 {
		cfg.WireCapBytes = DefaultWireCapBytes
	}
	if cfg.DecompressedCapBytes == 0 {
		cfg.DecompressedCapBytes = DefaultDecompressedCapBytes
	}
	if cfg.PlainTarCapBytes == 0 {
		cfg.PlainTarCapBytes = DefaultPlainTarCapBytes
	}
	if cfg.FileCountCap == 0 {
		cfg.FileCountCap = DefaultFileCountCap
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = DefaultHTTPTimeout
	}
	return &fetcher{cfg: cfg, client: newSecureClient(cfg)}
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
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
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
//
// Charset and length caps on name are explicitly deferred to sp-mwco.2.4 (agentinstall name-squat
// guard work) — not forgotten here, just out of this task's scope.
func sanitizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.NewReplacer(" ", "-", "_", "-").Replace(name)
	name = strings.Trim(name, "-.")
	return name
}

// maxDescriptionBytes caps a sanitized SKILL.md frontmatter description at 1 KiB (§4.9): Claude
// loads every installed skill's description into the system prompt at startup, so an unbounded,
// attacker-controlled description is a direct prompt-injection surface.
const maxDescriptionBytes = 1024

var (
	// tagMarkupRe strips XML/HTML-ish tag markup (e.g. "<system>", "</tool_use>").
	tagMarkupRe = regexp.MustCompile(`<[^>]*>`)
	// whitespaceRunRe collapses any run of whitespace (including newlines) to a single space.
	whitespaceRunRe = regexp.MustCompile(`\s+`)
)

// roleMarkerPatterns neutralizes reserved/role words and turn delimiters that a frontmatter
// description could use to inject a fake conversation turn once loaded into the system prompt
// (§4.9). This is defence-in-depth string scrubbing — a small, table-driven set of patterns — not
// a parser; it doesn't attempt to catch every possible injection technique.
var roleMarkerPatterns = []*regexp.Regexp{
	// "Human:", "Assistant:", "System:", "User:" (case-insensitive, standalone or leading).
	regexp.MustCompile(`(?i)\b(?:human|assistant|system|user)\s*:\s*`),
	// Llama/Mistral-style turn delimiters: "[INST]", "[/INST]", "[SYS]", "[/SYS]".
	regexp.MustCompile(`(?i)\[/?(?:inst|sys)\]`),
	// Alpaca/Vicuna-style "### Instruction:" section delimiters.
	regexp.MustCompile(`#{3,}\s*`),
}

// sanitizeDescription bounds and cleans an untrusted SKILL.md frontmatter description before it
// is ever persisted or shown to an agent (§4.9):
//  1. strip XML/tag markup
//  2. drop control characters (nothing below 0x20 survives — this becomes one line in a system prompt)
//  3. neutralize reserved/role markers and turn delimiters
//  4. collapse whitespace runs and trim
//  5. truncate to maxDescriptionBytes on a rune boundary
func sanitizeDescription(raw string) string {
	s := tagMarkupRe.ReplaceAllString(raw, "")
	s = stripControlChars(s)
	for _, re := range roleMarkerPatterns {
		s = re.ReplaceAllString(s, "")
	}
	s = strings.TrimSpace(whitespaceRunRe.ReplaceAllString(s, " "))
	return truncateRuneBoundary(s, maxDescriptionBytes)
}

// stripControlChars drops every rune below 0x20 (control characters) so nothing below 0x20
// survives, as required by §4.9. Whitespace-shaped control chars (tab/newline/CR/VT/FF) are
// replaced with a plain space rather than deleted outright — otherwise "line one\nline two"
// would collapse into the word-glued "line oneline two" once the newline vanishes, defeating the
// "reads as one line" goal step 4's whitespace-collapse is there to serve. Other control chars
// (e.g. NUL, BEL) carry no separating meaning and are dropped with no replacement.
func stripControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 {
			switch r {
			case '\t', '\n', '\r', '\v', '\f':
				b.WriteByte(' ')
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// truncateRuneBoundary truncates s to at most maxBytes bytes without splitting a multi-byte rune.
func truncateRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	b := s[:maxBytes]
	for len(b) > 0 {
		r, size := utf8.DecodeLastRuneInString(b)
		if r != utf8.RuneError || size != 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
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
	unpacked, err := f.client.fetchAndUnpack(ctx, rawURL, f.cfg.GitHubToken, subdir)
	if err != nil {
		return Result{}, err
	}
	entries := unpacked.entries
	var skippedEntries []string
	for _, se := range unpacked.skipped {
		skippedEntries = append(skippedEntries, fmt.Sprintf("%s (%s)", se.path, se.kind))
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
	if err := plainTarCapErr(int64(len(plainTar)), f.cfg.PlainTarCapBytes); err != nil {
		return Result{}, err
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
		SkippedEntries:  skippedEntries,
		SourceCommit:    unpacked.sourceCommit,
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
