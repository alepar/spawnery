package journal

import "testing"

func TestParseDurability(t *testing.T) {
	cases := map[string]DurabilityClass{
		"":             Ephemeral,
		"ephemeral":    Ephemeral,
		"node-local":   NodeLocal,
		"owner-sealed": OwnerSealed,
	}
	for in, want := range cases {
		got, err := ParseDurability(in)
		if err != nil {
			t.Fatalf("ParseDurability(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseDurability(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseDurability("nonsense"); err == nil {
		t.Fatal("expected error for unknown durability class")
	}
}

func TestDurabilityJournaled(t *testing.T) {
	if Ephemeral.Journaled() {
		t.Fatal("ephemeral must not be journaled")
	}
	if !NodeLocal.Journaled() || !OwnerSealed.Journaled() {
		t.Fatal("node-local and owner-sealed must be journaled")
	}
}

func TestMountShouldJournal(t *testing.T) {
	if (Mount{Class: Ephemeral}).shouldJournal() {
		t.Fatal("ephemeral mount must not journal")
	}
	if (Mount{Class: NodeLocal, Secret: true}).shouldJournal() {
		t.Fatal("secret mount must be excluded even when node-local")
	}
	if !(Mount{Class: NodeLocal}).shouldJournal() {
		t.Fatal("node-local non-secret mount must journal")
	}
}
