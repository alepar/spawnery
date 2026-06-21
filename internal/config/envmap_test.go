package config

import "testing"

func TestBuildEnvLayer(t *testing.T) {
	aliases := map[string]string{
		"CP_LISTEN":    "listen",
		"CP_STORE_DSN": "store.dsn",
		"CP_MAX_CONNS": "store.max_conns", // underscore leaf must survive (no _ -> . mangling)
	}
	env := map[string]string{
		"CP_LISTEN":    ":9000",
		"CP_MAX_CONNS": "10",
		// CP_STORE_DSN intentionally unset
	}
	got := buildEnvLayer(aliases, envFrom(env))

	if got["listen"] != ":9000" {
		t.Errorf("listen = %v, want :9000", got["listen"])
	}
	if got["store.max_conns"] != "10" {
		t.Errorf("store.max_conns = %v, want 10 (underscore leaf preserved)", got["store.max_conns"])
	}
	if _, present := got["store.dsn"]; present {
		t.Errorf("store.dsn should be absent (env var unset), got %v", got["store.dsn"])
	}
}

func TestBuildEnvLayer_EmptyValueIsSkipped(t *testing.T) {
	// A set-but-empty env var (e.g. FOO= from an unset shell var) must be treated as unset, so it
	// does not clobber a lower layer's default with "".
	got := buildEnvLayer(map[string]string{"CP_LISTEN": "listen"}, envFrom(map[string]string{"CP_LISTEN": ""}))
	if _, present := got["listen"]; present {
		t.Errorf("empty-valued env var should be skipped, got %v", got["listen"])
	}
}

func TestBuildEnvLayer_Empty(t *testing.T) {
	got := buildEnvLayer(map[string]string{"CP_X": "x"}, envFrom(nil))
	if len(got) != 0 {
		t.Errorf("expected empty layer when no env vars set, got %v", got)
	}
}
