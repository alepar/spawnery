package cp

import (
	"bytes"
	"testing"

	"connectrpc.com/connect"

	"spawnery/internal/cp/store"
)

func TestValidateCustomContent_Happy(t *testing.T) {
	cases := []struct {
		kind    store.ProfileEntryKind
		name    string
		content []byte
	}{
		{store.ProfileEntrySkill, "my-skill", []byte("content")},
		{store.ProfileEntryMCP, "my-mcp", []byte(`{"command":"docker"}`)},
		{store.ProfileEntryConfig, "config.json", []byte(`{}`)},
		{store.ProfileEntryPlugin, "my-plugin", []byte("plugin-content")},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind)+"/"+tc.name, func(t *testing.T) {
			if err := validateCustomContent(tc.kind, tc.name, tc.content); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateCustomContent_EmptyName(t *testing.T) {
	err := validateCustomContent(store.ProfileEntrySkill, "", []byte("data"))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestValidateCustomContent_PathSeparatorInName(t *testing.T) {
	for _, name := range []string{"foo/bar", "foo\\bar"} {
		err := validateCustomContent(store.ProfileEntrySkill, name, []byte("data"))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("name %q: expected InvalidArgument, got %v", name, err)
		}
	}
}

func TestValidateCustomContent_DotOrDotDotName(t *testing.T) {
	for _, name := range []string{".", ".."} {
		err := validateCustomContent(store.ProfileEntrySkill, name, []byte("data"))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("name %q: expected InvalidArgument, got %v", name, err)
		}
	}
}

func TestValidateCustomContent_AbsolutePathName(t *testing.T) {
	err := validateCustomContent(store.ProfileEntrySkill, "/absolute", []byte("data"))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for absolute path, got %v", err)
	}
}

func TestValidateCustomContent_EmptyContent(t *testing.T) {
	err := validateCustomContent(store.ProfileEntrySkill, "my-skill", nil)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for empty content, got %v", err)
	}

	err = validateCustomContent(store.ProfileEntrySkill, "my-skill", []byte{})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for empty content ([]byte{}), got %v", err)
	}
}

func TestValidateCustomContent_SizeCap(t *testing.T) {
	// Exactly at cap: ok.
	ok := bytes.Repeat([]byte("x"), maxArtifactInlineBytes)
	if err := validateCustomContent(store.ProfileEntrySkill, "my-skill", ok); err != nil {
		t.Errorf("at max size: unexpected error: %v", err)
	}

	// One byte over: rejected.
	over := bytes.Repeat([]byte("x"), maxArtifactInlineBytes+1)
	err := validateCustomContent(store.ProfileEntrySkill, "my-skill", over)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("over max size: expected InvalidArgument, got %v", err)
	}
}

func TestValidateCustomContent_UnsupportedKind(t *testing.T) {
	err := validateCustomContent("", "my-skill", []byte("data"))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument for unsupported kind, got %v", err)
	}
}

func TestEnforceProfileEntryCap_BelowCap(t *testing.T) {
	if err := enforceProfileEntryCap(maxArtifactsPerSpawn - 1); err != nil {
		t.Errorf("unexpected error below cap: %v", err)
	}
}

func TestEnforceProfileEntryCap_AtCap(t *testing.T) {
	err := enforceProfileEntryCap(maxArtifactsPerSpawn)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument at cap, got %v", err)
	}
}

func TestEnforceProfileEntryCap_Zero(t *testing.T) {
	if err := enforceProfileEntryCap(0); err != nil {
		t.Errorf("unexpected error for 0 existing: %v", err)
	}
}
