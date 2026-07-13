package skillfetch

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// ExtractSkillMD zstd-decodes compressed (a canonical member tar, as produced by
// FetchBundle/FetchBundleConditional and stored content-addressed in Garage) under a size cap and
// returns the content of its top-level SKILL.md entry.
//
// capBytes <= 0 falls back to DefaultPlainTarCapBytes. The decode is bounded by an
// io.LimitedReader — this content is untrusted (it round-trips through Garage and, transitively,
// an arbitrary GitHub repo), so the bound is mandatory, mirroring the same cap enforced at ingest
// (skillfetch.go's plainTarCapErr).
//
// Returns an error, never a nil result: a corrupt zstd stream, a plain tar exceeding capBytes, or
// a tar with no SKILL.md entry at its root all fail loud (used by GetBundleDiff, sp-mwco.1.7 §4.9
// — a diff must be able to say "this member's body is unavailable" rather than silently omit it).
func ExtractSkillMD(compressed []byte, capBytes int64) ([]byte, error) {
	if capBytes <= 0 {
		capBytes = DefaultPlainTarCapBytes
	}
	dec, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("skillfetch: invalid zstd stream: %w", err)
	}
	defer dec.Close()

	lr := &io.LimitedReader{R: dec, N: capBytes + 1}
	plain, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("skillfetch: decompress: %w", err)
	}
	if int64(len(plain)) > capBytes {
		return nil, fmt.Errorf("skillfetch: skill tar exceeds size cap (%d bytes)", capBytes)
	}

	tr := tar.NewReader(bytes.NewReader(plain))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("skillfetch: tar read: %w", err)
		}
		if hdr.Name != "SKILL.md" {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("skillfetch: read SKILL.md: %w", err)
		}
		return content, nil
	}
	return nil, fmt.Errorf("skillfetch: no SKILL.md found in tar")
}
