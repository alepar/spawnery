package cp

import "testing"

func TestNextSpawnName(t *testing.T) {
	cases := []struct {
		base  string
		taken []string
		want  string
	}{
		{"Wiki", nil, "Wiki"},
		{"Wiki", []string{"Wiki"}, "Wiki 2"},
		{"Wiki", []string{"Wiki", "Wiki 2"}, "Wiki 3"},
		{"Wiki", []string{"Wiki", "Wiki 3"}, "Wiki 2"}, // fills the first gap
		{"", nil, "spawn"},
		{"", []string{"spawn"}, "spawn 2"},
	}
	for _, c := range cases {
		taken := map[string]bool{}
		for _, n := range c.taken {
			taken[n] = true
		}
		if got := nextSpawnName(c.base, taken); got != c.want {
			t.Errorf("nextSpawnName(%q, %v) = %q, want %q", c.base, c.taken, got, c.want)
		}
	}
}
