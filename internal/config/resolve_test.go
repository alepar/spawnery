package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRef(t *testing.T) {
	tests := []struct {
		in          string
		scheme, arg string
		isRef       bool
	}{
		{"${env:FOO}", "env", "FOO", true},
		{"${file:/a/b.pem}", "file", "/a/b.pem", true},
		{"${sops:store.dsn}", "sops", "store.dsn", true},
		{"${env:}", "env", "", true}, // empty arg is a ref; the resolver decides if it's valid
		{"literal", "", "", false},
		{"prefix-${env:X}", "", "", false}, // only whole-value refs resolve
		{"${notclosed", "", "", false},
		{"${noarg}", "", "", false}, // missing ':'
	}
	for _, tc := range tests {
		s, a, ok := parseRef(tc.in)
		if ok != tc.isRef || s != tc.scheme || a != tc.arg {
			t.Errorf("parseRef(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.in, s, a, ok, tc.scheme, tc.arg, tc.isRef)
		}
	}
}

func TestEnvResolver(t *testing.T) {
	r := newEnvResolver(envFrom(map[string]string{"FOO": "bar"}))
	if r.Scheme() != "env" {
		t.Errorf("scheme = %q", r.Scheme())
	}
	if v, err := r.Resolve("FOO"); err != nil || v != "bar" {
		t.Errorf("Resolve(FOO) = (%q,%v), want (bar,nil)", v, err)
	}
	if _, err := r.Resolve("MISSING"); err == nil {
		t.Error("expected error resolving unset env var")
	}
}

func TestFileResolver(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret")
	if err := os.WriteFile(p, []byte("  s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newFileResolver()
	if v, err := r.Resolve(p); err != nil || v != "s3cr3t" {
		t.Errorf("Resolve = (%q,%v), want (s3cr3t,nil) (trimmed)", v, err)
	}
	if _, err := r.Resolve(filepath.Join(dir, "nope")); err == nil {
		t.Error("expected error resolving missing file")
	}
}

func TestResolveRefs(t *testing.T) {
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "k.pem")
	if err := os.WriteFile(pemPath, []byte("PEMDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := kFromFlat(map[string]any{
		"a.token": "${env:TOK}",
		"a.key":   "${file:" + pemPath + "}",
		"plain":   "untouched",
		"port":    8080, // non-string leaf left alone
	})
	resolvers := map[string]Resolver{
		"env":  newEnvResolver(envFrom(map[string]string{"TOK": "xyz"})),
		"file": newFileResolver(),
	}
	if err := resolveRefs(k, resolvers); err != nil {
		t.Fatalf("resolveRefs: %v", err)
	}
	if got := k.String("a.token"); got != "xyz" {
		t.Errorf("a.token = %q, want xyz", got)
	}
	if got := k.String("a.key"); got != "PEMDATA" {
		t.Errorf("a.key = %q, want PEMDATA", got)
	}
	if got := k.String("plain"); got != "untouched" {
		t.Errorf("plain = %q, want untouched", got)
	}
}

func TestResolveRefs_UnknownSchemeIsFatal(t *testing.T) {
	k := kFromFlat(map[string]any{"x": "${vault:foo}"})
	if err := resolveRefs(k, map[string]Resolver{}); err == nil {
		t.Fatal("expected error for unknown scheme, got nil")
	}
}

func TestResolveRefs_ResolverErrorIsFatal(t *testing.T) {
	k := kFromFlat(map[string]any{"x": "${env:MISSING}"})
	resolvers := map[string]Resolver{"env": newEnvResolver(envFrom(nil))}
	if err := resolveRefs(k, resolvers); err == nil {
		t.Fatal("expected fatal error for unresolvable ${env:MISSING}, got nil")
	}
}
