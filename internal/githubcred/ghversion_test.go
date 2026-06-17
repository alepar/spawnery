package githubcred

import (
	"testing"
)

func TestParseGHVersionFullOutput(t *testing.T) {
	cases := []struct {
		input string
		want  [3]int
	}{
		{"gh version 2.63.0 (2024-11-27)\nhttps://github.com/cli/cli/releases/tag/v2.63.0", [3]int{2, 63, 0}},
		{"gh version 2.64.1 (2025-01-10)\nhttps://github.com/cli/cli/releases/tag/v2.64.1", [3]int{2, 64, 1}},
		{"gh version 3.0.0 (2025-06-01)", [3]int{3, 0, 0}},
		{"gh version 2.62.9 (2024-10-01)", [3]int{2, 62, 9}},
		{"2.63.0", [3]int{2, 63, 0}},
	}
	for _, tc := range cases {
		got, err := ParseGHVersion(tc.input)
		if err != nil {
			t.Errorf("ParseGHVersion(%q) err = %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseGHVersion(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestParseGHVersionRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "gh version", "not a version", "abc.def.ghi"} {
		if _, err := ParseGHVersion(bad); err == nil {
			t.Errorf("ParseGHVersion(%q) err = nil, want error", bad)
		}
	}
}

func TestGHVersionAtLeast(t *testing.T) {
	cases := []struct {
		a, b [3]int
		want bool
	}{
		{[3]int{2, 63, 0}, [3]int{2, 63, 0}, true},  // equal → satisfies
		{[3]int{2, 63, 1}, [3]int{2, 63, 0}, true},  // patch higher
		{[3]int{2, 64, 0}, [3]int{2, 63, 0}, true},  // minor higher
		{[3]int{3, 0, 0}, [3]int{2, 63, 0}, true},   // major higher
		{[3]int{2, 62, 9}, [3]int{2, 63, 0}, false}, // minor lower → below min
		{[3]int{2, 63, 0}, [3]int{2, 63, 1}, false}, // patch lower
		{[3]int{1, 99, 99}, [3]int{2, 63, 0}, false}, // major lower
	}
	for _, tc := range cases {
		if got := GHVersionAtLeast(tc.a, tc.b); got != tc.want {
			t.Errorf("GHVersionAtLeast(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMinGHVersionIsAtLeast2_63_0(t *testing.T) {
	// Ensure MinGHVersion constant is never accidentally lowered below the CVE fix.
	v, err := ParseGHVersion(MinGHVersion)
	if err != nil {
		t.Fatalf("ParseGHVersion(MinGHVersion=%q) err = %v", MinGHVersion, err)
	}
	cve := [3]int{2, 63, 0}
	if !GHVersionAtLeast(v, cve) {
		t.Fatalf("MinGHVersion %q (%v) is below CVE-2024-53858 fix version %v; do not lower this constant",
			MinGHVersion, v, cve)
	}
}
