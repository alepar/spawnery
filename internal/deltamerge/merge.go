// Package deltamerge merges N OCI delta layer tars into a single deterministic tar.
// It is used by the squash path to collapse a chain of capture deltas into one layer,
// and as a scrub filter to strip noise paths (apt caches, /tmp) before a commit.
//
// Whiteout semantics follow the OCI image specification:
//   - .wh.<name> in dir D deletes D/<name>
//   - .wh..wh..opq in dir D (opaque whiteout) masks all earlier dir contents
//
// BasePaths controls whiteout-of-base preservation: a whiteout targeting a path that
// only the base image contains (not any earlier delta layer) is KEPT in the output so
// downstream consumers know to mask that base content.  A whiteout deleting nothing
// (not in accumulator, not in BasePaths) is dropped.
package deltamerge

import (
	"archive/tar"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// fixedMtime is the normalized mtime applied to all output entries for determinism.
var fixedMtime = time.Unix(0, 0).UTC()

// Options controls merge behavior.
type Options struct {
	// ScrubPrefixes are absolute path prefixes to exclude from the output.
	// e.g. []string{"/var/cache/apt", "/var/lib/apt/lists", "/tmp"}.
	// An entry whose name (or any ancestor) matches a prefix is dropped;
	// whiteouts targeting scrubbed paths are dropped too.
	ScrubPrefixes []string

	// BasePaths is the set of absolute paths present in the base image layers
	// (keys like "/etc/passwd").  Whiteouts that target a path in BasePaths
	// are preserved in the output — they must mask base content downstream.
	// Whiteouts targeting only accumulated-layer paths are dropped after apply.
	BasePaths map[string]struct{}
}

// entry holds one collected tar entry (nil hdr means "was whiteout-deleted").
type entry struct {
	hdr     *tar.Header
	payload []byte // nil for non-regular-file types
}

// Merge merges N OCI-format layer tars (applied in order) into a single
// deterministic tar written to out.  Later layers override earlier ones.
// Whiteout handling follows OCI image spec §6.  ScrubPrefixes paths are stripped.
func Merge(layers []io.Reader, opts Options, out io.Writer) error {
	// acc: normalized path (no leading "./", no leading "/", no trailing "/") → entry.
	acc := map[string]*entry{}

	for _, r := range layers {
		tr := tar.NewReader(r)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("read layer tar: %w", err)
			}

			name := normalizePath(hdr.Name)
			dir, base := path.Dir(name), path.Base(name)

			// ── opaque whiteout (.wh..wh..opq) ──────────────────────────────
			if base == ".wh..wh..opq" {
				// Opaque whiteout in a scrubbed dir: drop the marker (scrubbed anyway).
				if opts.isScrubbed(dir) {
					continue
				}
				// Remove all accumulated entries that live under dir/.
				prefix := dir + "/"
				if dir == "." {
					prefix = ""
				}
				for k := range acc {
					if dir == "." || strings.HasPrefix(k, prefix) {
						delete(acc, k)
					}
				}
				// Keep the opaque marker in output only if the base image has entries
				// under this dir (otherwise there's nothing in the base to mask).
				if opts.baseDirHasEntries(dir) {
					hdr.Name = name
					acc[name] = &entry{hdr: cloneHeader(hdr)}
				}
				continue
			}

			// ── regular whiteout (.wh.<target>) ─────────────────────────────
			if strings.HasPrefix(base, ".wh.") {
				target := path.Join(dir, base[len(".wh."):])
				// Drop if the whiteout entry itself is in a scrubbed dir.
				if opts.isScrubbed(name) {
					continue
				}
				// Drop if the target is a scrubbed path (we'd never emit target anyway).
				if opts.isScrubbed(target) {
					continue
				}
				// Remove target AND all of its children from the accumulator.
				// OCI whiteout of a directory masks everything under it, so a merged
				// layer must not emit any accumulated descendant of the whited-out dir.
				// (Finding: the original code only deleted the exact key, leaving
				// children — e.g. opt/tool/bin — alive in the accumulator.)
				targetPrefix := target + "/"
				delete(acc, target)
				for k := range acc {
					if strings.HasPrefix(k, targetPrefix) {
						delete(acc, k)
					}
				}
				// Keep the whiteout if the base image contains the target itself OR
				// any descendant — the merged layer must still mask that base content.
				// This check runs AFTER the acc deletion so it is not short-circuited
				// by an earlier-delta-layer touch of the same path (finding: the old
				// code skipped baseContains when acc[target] existed, so a path in both
				// an earlier delta and the base lost its whiteout in the squashed layer,
				// letting the base content resurface on apply).
				if opts.baseContains(target) || opts.baseDirHasEntries(target) {
					hdr.Name = name
					acc[name] = &entry{hdr: cloneHeader(hdr)}
					continue
				}
				// Target in neither accumulator nor BasePaths → drop (deletes nothing).
				continue
			}

			// ── regular entry ────────────────────────────────────────────────
			if opts.isScrubbed(name) {
				continue
			}
			var payload []byte
			if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
				payload, err = io.ReadAll(tr)
				if err != nil {
					return fmt.Errorf("read entry payload %s: %w", name, err)
				}
			}
			hdr.Name = name
			acc[name] = &entry{hdr: cloneHeader(hdr), payload: payload}
		}
	}

	// Emit in sorted path order for golden reproducibility.
	paths := make([]string, 0, len(acc))
	for p := range acc {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	tw := tar.NewWriter(out)
	for _, p := range paths {
		e := acc[p]
		if e == nil || e.hdr == nil {
			continue
		}
		// Normalise timestamps for determinism.
		e.hdr.ModTime = fixedMtime
		e.hdr.AccessTime = time.Time{}
		e.hdr.ChangeTime = time.Time{}
		e.hdr.Name = p
		if err := tw.WriteHeader(e.hdr); err != nil {
			return fmt.Errorf("write header %s: %w", p, err)
		}
		if len(e.payload) > 0 {
			if _, err := tw.Write(e.payload); err != nil {
				return fmt.Errorf("write payload %s: %w", p, err)
			}
		}
	}
	return tw.Close()
}

// ParseWhiteout parses a (normalized, no-leading-slash) tar entry name and reports
// whether it is an OCI whiteout.  Returns:
//
//	dir    – the directory containing the whiteout
//	target – the path the whiteout targets (empty for opaque whiteouts)
//	opaque – true if .wh..wh..opq
//	ok     – true if this is any kind of whiteout
func ParseWhiteout(name string) (dir, target string, opaque, ok bool) {
	name = normalizePath(name)
	base := path.Base(name)
	d := path.Dir(name)
	if base == ".wh..wh..opq" {
		return d, "", true, true
	}
	if strings.HasPrefix(base, ".wh.") {
		t := path.Join(d, base[len(".wh."):])
		return d, t, false, true
	}
	return "", "", false, false
}

// normalizePath strips leading "./" and "/" and trailing "/" from a tar path name,
// producing a canonical relative path suitable for accumulator keys.
func normalizePath(name string) string {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimPrefix(name, "/")
	name = strings.TrimRight(name, "/")
	return name
}

// isScrubbed reports whether the entry at (normalized, no-leading-slash) name
// matches any scrub prefix.
func (o *Options) isScrubbed(name string) bool {
	for _, pfx := range o.ScrubPrefixes {
		pfx = strings.TrimPrefix(pfx, "/")
		pfx = strings.TrimRight(pfx, "/")
		if name == pfx || strings.HasPrefix(name, pfx+"/") {
			return true
		}
	}
	return false
}

// baseContains reports whether the normalized path is in BasePaths.
// BasePaths keys have a leading "/" (e.g. "/etc/passwd"); we strip it for comparison.
func (o *Options) baseContains(name string) bool {
	if o.BasePaths == nil {
		return false
	}
	_, ok := o.BasePaths["/"+name]
	if !ok {
		// Also check without leading slash in case caller omitted it.
		_, ok = o.BasePaths[name]
	}
	return ok
}

// baseDirHasEntries reports whether any BasePaths entry is a descendant of dir.
// dir is a normalized path (no leading slash, no trailing slash); "." means root.
func (o *Options) baseDirHasEntries(dir string) bool {
	if len(o.BasePaths) == 0 {
		return false
	}
	for p := range o.BasePaths {
		norm := strings.TrimPrefix(p, "/")
		norm = strings.TrimRight(norm, "/")
		if dir == "." || dir == "" {
			// Root: any non-empty BasePaths entry qualifies.
			if norm != "" {
				return true
			}
		} else {
			if strings.HasPrefix(norm, dir+"/") {
				return true
			}
		}
	}
	return false
}

// cloneHeader returns a shallow copy of hdr so we don't accidentally alias the
// original tar.Header (which tar.Reader may reuse across calls).
func cloneHeader(hdr *tar.Header) *tar.Header {
	cp := *hdr
	return &cp
}
