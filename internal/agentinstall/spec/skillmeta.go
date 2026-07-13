package spec

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxDescriptionBytes caps a sanitized SKILL.md frontmatter description at 1 KiB (§4.9):
// every harness loads each installed skill's description into the system prompt at startup,
// so an unbounded, attacker-controlled description is a direct prompt-injection surface.
const MaxDescriptionBytes = 1024

// MaxSkillNameBytes caps a SKILL.md frontmatter/skill name at 64 bytes (§4.9): the name is
// loaded into the prompt alongside the description, so it gets the same treatment.
const MaxSkillNameBytes = 64

var (
	// tagMarkupRe strips XML/HTML-ish tag markup (e.g. "<system>", "</tool_use>").
	tagMarkupRe = regexp.MustCompile(`<[^>]*>`)
	// whitespaceRunRe collapses any run of whitespace (including newlines) to a single space.
	whitespaceRunRe = regexp.MustCompile(`\s+`)
)

// roleMarkerPatterns neutralizes reserved/role words and turn delimiters that an untrusted
// description could use to inject a fake conversation turn once loaded into a system prompt
// (§4.9). This is defence-in-depth string scrubbing — a small, table-driven set of patterns —
// not a parser; it doesn't attempt to catch every possible injection technique.
var roleMarkerPatterns = []*regexp.Regexp{
	// "Human:", "Assistant:", "System:", "User:" (case-insensitive, standalone or leading).
	regexp.MustCompile(`(?i)\b(?:human|assistant|system|user)\s*:\s*`),
	// Llama/Mistral-style turn delimiters: "[INST]", "[/INST]", "[SYS]", "[/SYS]".
	regexp.MustCompile(`(?i)\[/?(?:inst|sys)\]`),
	// Alpaca/Vicuna-style "### Instruction:" section delimiters.
	regexp.MustCompile(`#{3,}\s*`),
}

// SanitizeDescription bounds and cleans an untrusted SKILL.md frontmatter description before
// it is ever loaded into a system prompt (§4.9):
//  1. strip XML/tag markup
//  2. drop control characters (nothing below 0x20 survives — this becomes one line in a system prompt)
//  3. neutralize reserved/role markers and turn delimiters
//  4. collapse whitespace runs and trim
//  5. truncate to MaxDescriptionBytes on a rune boundary
//
// Shared by internal/cp/skillfetch (the catalog row's Description, sanitized at fetch time)
// and internal/agentinstall (the installed SKILL.md's frontmatter description — what every
// harness actually loads — sanitized at install time). agentinstall is a leaf package (see
// imports_test.go) that cannot import skillfetch, so this pure string sanitizer lives here,
// in the stdlib-only spec package both sides can import; skillfetch.sanitizeDescription is a
// thin delegating wrapper (sp-mwco.1.11).
func SanitizeDescription(raw string) string {
	s := tagMarkupRe.ReplaceAllString(raw, "")
	s = stripControlChars(s)
	for _, re := range roleMarkerPatterns {
		s = re.ReplaceAllString(s, "")
	}
	s = strings.TrimSpace(whitespaceRunRe.ReplaceAllString(s, " "))
	return truncateRuneBoundary(s, MaxDescriptionBytes)
}

// stripControlChars drops every rune below 0x20 (control characters) so nothing below 0x20
// survives, as required by §4.9. Whitespace-shaped control chars (tab/newline/CR/VT/FF) are
// replaced with a plain space rather than deleted outright — otherwise "line one\nline two"
// would collapse into the word-glued "line oneline two" once the newline vanishes, defeating
// the "reads as one line" goal step 4's whitespace-collapse is there to serve. Other control
// chars (e.g. NUL, BEL) carry no separating meaning and are dropped with no replacement.
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
