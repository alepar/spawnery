package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSecret_RedactsEverywhere(t *testing.T) {
	s := Secret("hunter2")

	// The real value is still usable by code.
	if string(s) != "hunter2" {
		t.Fatalf("string(s) = %q, want hunter2", string(s))
	}

	cases := map[string]string{
		"String()": s.String(),
		"%v":       fmt.Sprintf("%v", s),
		"%s":       fmt.Sprintf("%s", s),
		"%+v":      fmt.Sprintf("%+v", s),
		"%#v":      fmt.Sprintf("%#v", s),
		"%d-ish":   fmt.Sprintf("%q", s),
	}
	for name, got := range cases {
		if strings.Contains(got, "hunter2") {
			t.Errorf("%s leaked the secret: %q", name, got)
		}
		if !strings.Contains(got, "***") {
			t.Errorf("%s = %q, want redaction ***", name, got)
		}
	}

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"***"` {
		t.Errorf("json = %s, want \"***\"", b)
	}

	y, err := yaml.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(y), "hunter2") || strings.TrimSpace(string(y)) != "'***'" && strings.TrimSpace(string(y)) != "***" {
		t.Errorf("yaml = %q, want redacted ***", string(y))
	}
}

// A secret nested in a struct must not leak via %+v on the parent.
func TestSecret_RedactsInStruct(t *testing.T) {
	type holder struct {
		Name string
		Key  Secret
	}
	h := holder{Name: "db", Key: Secret("p@ss")}
	got := fmt.Sprintf("%+v", h)
	if strings.Contains(got, "p@ss") {
		t.Errorf("struct %%+v leaked secret: %q", got)
	}
}
