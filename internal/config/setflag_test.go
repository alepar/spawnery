package config

import "testing"

func TestParseSets(t *testing.T) {
	got, err := parseSets([]string{"store.dsn=postgres://x", "listen=:8080"})
	if err != nil {
		t.Fatalf("parseSets: %v", err)
	}
	if got["store.dsn"] != "postgres://x" {
		t.Errorf("store.dsn = %v, want postgres://x", got["store.dsn"])
	}
	if got["listen"] != ":8080" {
		t.Errorf("listen = %v, want :8080 (scalar kept as string, no YAML coercion)", got["listen"])
	}
}

func TestParseSets_SplitsOnFirstEquals(t *testing.T) {
	got, err := parseSets([]string{"a.b=x=y"})
	if err != nil {
		t.Fatalf("parseSets: %v", err)
	}
	if got["a.b"] != "x=y" {
		t.Errorf("a.b = %v, want x=y (only first = splits)", got["a.b"])
	}
}

func TestParseSets_EmptyValueAllowed(t *testing.T) {
	got, err := parseSets([]string{"k="})
	if err != nil {
		t.Fatalf("parseSets: %v", err)
	}
	if v, ok := got["k"]; !ok || v != "" {
		t.Errorf("k = %v (ok=%v), want empty string present", v, ok)
	}
}

func TestParseSets_MissingEqualsIsError(t *testing.T) {
	if _, err := parseSets([]string{"noequals"}); err == nil {
		t.Fatal("expected error for --set value without =, got nil")
	}
}

func TestParseSets_EmptyKeyIsError(t *testing.T) {
	if _, err := parseSets([]string{"=v"}); err == nil {
		t.Fatal("expected error for --set with empty key, got nil")
	}
}
