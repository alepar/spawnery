package skillfetch

import (
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// buildMemberTarZst builds a canonical member tar.zst (the same shape FetchBundle/
// FetchBundleConditional produce and skillstore persists) from the given entries.
func buildMemberTarZst(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	plain, err := canonicalRepack(entries)
	if err != nil {
		t.Fatalf("canonicalRepack: %v", err)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	return enc.EncodeAll(plain, nil)
}

func TestExtractSkillMD_HappyPath(t *testing.T) {
	body := "---\nname: my-skill\n---\n# My Skill\nDo the thing."
	compressed := buildMemberTarZst(t, []tarEntry{
		{path: "SKILL.md", content: []byte(body)},
		{path: "helper.py", content: []byte("print('hi')")},
	})

	got, err := ExtractSkillMD(compressed, 0)
	if err != nil {
		t.Fatalf("ExtractSkillMD: %v", err)
	}
	if string(got) != body {
		t.Fatalf("got %q, want %q", got, body)
	}
}

func TestExtractSkillMD_NoSkillMD_Errors(t *testing.T) {
	compressed := buildMemberTarZst(t, []tarEntry{
		{path: "README.md", content: []byte("just a readme")},
	})

	_, err := ExtractSkillMD(compressed, 0)
	if err == nil {
		t.Fatal("expected an error for a tar with no SKILL.md")
	}
	if !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("error does not mention SKILL.md: %v", err)
	}
}

func TestExtractSkillMD_ExceedsCap_Errors(t *testing.T) {
	body := strings.Repeat("x", 1024)
	compressed := buildMemberTarZst(t, []tarEntry{
		{path: "SKILL.md", content: []byte(body)},
	})

	_, err := ExtractSkillMD(compressed, 16) // far below the plain tar's actual size
	if err == nil {
		t.Fatal("expected an error for a tar exceeding the cap")
	}
	if !strings.Contains(err.Error(), "size cap") || !strings.Contains(err.Error(), "16") {
		t.Fatalf("error does not name the cap: %v", err)
	}
}

func TestExtractSkillMD_CorruptZstd_Errors(t *testing.T) {
	_, err := ExtractSkillMD([]byte("not a zstd stream at all"), 0)
	if err == nil {
		t.Fatal("expected an error for a corrupt zstd stream")
	}
	if !strings.Contains(err.Error(), "skillfetch:") {
		t.Fatalf("error not wrapped with skillfetch context: %v", err)
	}
}
