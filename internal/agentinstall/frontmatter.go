package agentinstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"spawnery/internal/agentinstall/spec"
)

// sanitizeSkillMD rewrites <dir>/SKILL.md's YAML frontmatter in place so that the description
// and name a harness loads into its system prompt are bounded, the same way the catalog row
// already is (skillfetch.sanitizeDescription) — see sp-mwco.1.11. It is called on the STAGED
// temp copy of a skill tree, after copyTree and before the atomic rename, so the source tree
// (and the stored tar / its sha, which canonicalRepack ships byte-for-byte on purpose) is never
// touched.
//
// Only the "description" and "name" values are rewritten, in place; no key is added or removed,
// and every other key (allowed-tools, license, comments, …) round-trips through the parsed
// yaml.Node unchanged. The body (everything after the closing "---" delimiter) is preserved
// byte-for-byte — it is never parsed.
//
// changed is false, and the file is left untouched, when:
//   - SKILL.md has no leading "---" frontmatter block at all (nothing reaches a system prompt
//     that this function would need to bound), or
//   - the frontmatter is a well-formed YAML mapping whose description/name (if present) already
//     equal their sanitized/pinned form (idempotency: re-sanitizing an already-sanitized file is
//     a no-op).
//
// A frontmatter block that starts (a leading "---" line) but cannot be safely bounded — no
// closing delimiter, or the block does not parse as a YAML mapping — is a hard error: fail
// closed rather than let unbounded content reach a system prompt.
func sanitizeSkillMD(dir, name string) (changed bool, err error) {
	path := filepath.Join(dir, "SKILL.md")

	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("stat SKILL.md: %w", err)
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read SKILL.md: %w", err)
	}

	yamlPart, body, hasFrontmatter, err := splitFrontmatter(orig)
	if err != nil {
		return false, fmt.Errorf("SKILL.md: %w", err)
	}
	if !hasFrontmatter {
		return false, nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(yamlPart), &doc); err != nil {
		return false, fmt.Errorf("SKILL.md: parse frontmatter: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return false, fmt.Errorf("SKILL.md: frontmatter is not a YAML mapping")
	}
	mapping := doc.Content[0]

	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		val := mapping.Content[i+1]
		switch key.Value {
		case "description":
			if rewriteScalar(val, spec.SanitizeDescription(val.Value)) {
				changed = true
			}
		case "name":
			if rewriteScalar(val, name) {
				changed = true
			}
		}
	}
	if !changed {
		return false, nil
	}

	var out strings.Builder
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(mapping); err != nil {
		return false, fmt.Errorf("SKILL.md: re-encode frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return false, fmt.Errorf("SKILL.md: re-encode frontmatter: %w", err)
	}

	rebuilt := "---\n" + out.String() + "---\n" + body
	if err := os.WriteFile(path, []byte(rebuilt), info.Mode().Perm()); err != nil {
		return false, fmt.Errorf("write sanitized SKILL.md: %w", err)
	}
	return true, nil
}

// rewriteScalar overwrites val — a description or name node, of ANY node kind — with a
// single-line double-quoted scalar holding newValue, returning whether that was actually a
// change from val's prior value. A description/name that came in as an unquoted plain scalar, a
// single- or double-quoted scalar, a multi-line block/folded scalar, or even a malformed
// non-scalar (mapping/sequence) are all normalized the same way, so the rewritten frontmatter
// always reads as one bounded line no matter the source shape — there is nothing left in the
// old structure worth preserving once its value has been replaced.
//
// changed compares VALUE only, never Style/Tag: an already-sanitized plain (unquoted) scalar
// whose value already equals newValue is not a change, and must not be rewritten — that is what
// keeps an already-sanitized file byte-identical (changed=false, sanitizeSkillMD skips the
// write). A non-scalar node has no comparable Value (it's always ""), so it is always treated as
// changed.
func rewriteScalar(val *yaml.Node, newValue string) (changed bool) {
	changed = val.Kind != yaml.ScalarNode || val.Value != newValue
	val.SetString(newValue)
	val.Style = yaml.DoubleQuotedStyle
	return changed
}

// splitFrontmatter splits SKILL.md content into (yamlPart, body). hasFrontmatter is false, with
// no error, when content does not begin with a standalone "---" delimiter line — the no-op case
// (nothing reaches a system prompt for sanitizeSkillMD to bound).
//
// err is non-nil when a leading "---" delimiter line is present but never closed by another
// standalone "---" line, or the closing delimiter line has trailing content — a frontmatter
// block we cannot safely bound.
func splitFrontmatter(content []byte) (yamlPart, body string, hasFrontmatter bool, err error) {
	s := string(content)
	if !strings.HasPrefix(s, "---") {
		return "", s, false, nil
	}
	rest := strings.TrimPrefix(s[3:], "\r")
	if !strings.HasPrefix(rest, "\n") {
		// "---" is not actually a standalone delimiter line (e.g. "---foo"); not frontmatter.
		return "", s, false, nil
	}
	rest = rest[1:]

	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", "", false, fmt.Errorf("frontmatter has no closing '---' delimiter")
	}
	yamlPart = rest[:idx]
	after := strings.TrimPrefix(rest[idx+len("\n---"):], "\r")
	switch {
	case after == "":
		body = ""
	case strings.HasPrefix(after, "\n"):
		body = after[1:]
	default:
		return "", "", false, fmt.Errorf("frontmatter closing '---' delimiter has trailing content on its line")
	}
	return yamlPart, body, true, nil
}
