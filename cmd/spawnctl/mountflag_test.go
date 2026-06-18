package main

import "testing"

func TestParseMountFlag(t *testing.T) {
	tests := []struct {
		name       string
		spec       string
		wantName   string
		wantURI    string
		wantCreate bool
		wantErr    bool
	}{
		{name: "github slot with create", spec: "repo=github:octo/demo,create", wantName: "repo", wantURI: "github:octo/demo", wantCreate: true},
		{name: "github slot no create", spec: "repo=github:octo/demo", wantName: "repo", wantURI: "github:octo/demo", wantCreate: false},
		{name: "whitespace trimmed", spec: " repo = github:octo/demo , create ", wantName: "repo", wantURI: "github:octo/demo", wantCreate: true},
		{name: "trailing comma tolerated", spec: "repo=github:octo/demo,", wantName: "repo", wantURI: "github:octo/demo"},
		{name: "non-github backend passthrough", spec: "data=scratch", wantName: "data", wantURI: "scratch"},
		{name: "missing equals", spec: "repo", wantErr: true},
		{name: "empty name", spec: "=github:octo/demo", wantErr: true},
		{name: "empty backend", spec: "repo=", wantErr: true},
		{name: "empty backend with option", spec: "repo=,create", wantErr: true},
		{name: "unknown option", spec: "repo=github:octo/demo,foo", wantErr: true},
		{name: "bad github uri owner only", spec: "repo=github:owneronly", wantErr: true},
		{name: "bad github uri trailing path", spec: "repo=github:octo/demo/extra", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mb, err := parseMountFlag(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMountFlag(%q) = %+v, want error", tt.spec, mb)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMountFlag(%q): unexpected error: %v", tt.spec, err)
			}
			if mb.GetName() != tt.wantName || mb.GetBackendUri() != tt.wantURI || mb.GetCreateIfMissing() != tt.wantCreate {
				t.Fatalf("parseMountFlag(%q) = {name:%q uri:%q create:%v}, want {name:%q uri:%q create:%v}",
					tt.spec, mb.GetName(), mb.GetBackendUri(), mb.GetCreateIfMissing(), tt.wantName, tt.wantURI, tt.wantCreate)
			}
			// Containment/D1: the client never names a credential.
			if mb.GetCredentialSecretId() != "" {
				t.Fatalf("parseMountFlag(%q): credential_secret_id must be empty, got %q", tt.spec, mb.GetCredentialSecretId())
			}
		})
	}
}

func TestParseMountFlags(t *testing.T) {
	t.Run("empty input -> nil", func(t *testing.T) {
		got, err := parseMountFlags(nil)
		if err != nil || got != nil {
			t.Fatalf("parseMountFlags(nil) = (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("multiple distinct mounts", func(t *testing.T) {
		got, err := parseMountFlags([]string{"repo=github:octo/demo,create", "data=scratch"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0].GetName() != "repo" || got[1].GetName() != "data" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("duplicate name rejected", func(t *testing.T) {
		if _, err := parseMountFlags([]string{"repo=github:a/b", "repo=github:c/d"}); err == nil {
			t.Fatal("want error for duplicate mount name")
		}
	})
	t.Run("propagates parse error", func(t *testing.T) {
		if _, err := parseMountFlags([]string{"bad"}); err == nil {
			t.Fatal("want error for malformed spec")
		}
	})
}
