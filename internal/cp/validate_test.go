package cp

import (
	"testing"

	cpv1 "spawnery/gen/cp/v1"
)

func validManifest() *cpv1.AppManifest {
	return &cpv1.AppManifest{
		ApiVersion: "spawnery/v1", Id: "alice/wiki", Title: "Wiki", Visibility: "open",
		Mounts: []*cpv1.ManifestMount{{Name: "main", Path: "data", Seed: "seed"}},
	}
}

func TestValidateManifest(t *testing.T) {
	if err := validateManifest(validManifest(), "1.0.0", "alice/wiki@sha"); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	cases := []struct {
		name    string
		mutate  func(*cpv1.AppManifest)
		version string
		ref     string
	}{
		{"bad apiVersion", func(m *cpv1.AppManifest) { m.ApiVersion = "spawnery/v2" }, "1.0.0", "r"},
		{"empty id", func(m *cpv1.AppManifest) { m.Id = "" }, "1.0.0", "r"},
		{"id no slash", func(m *cpv1.AppManifest) { m.Id = "wiki" }, "1.0.0", "r"},
		{"id two slashes", func(m *cpv1.AppManifest) { m.Id = "a/b/c" }, "1.0.0", "r"},
		{"empty title", func(m *cpv1.AppManifest) { m.Title = "" }, "1.0.0", "r"},
		{"bad semver", func(m *cpv1.AppManifest) {}, "v1", "r"},
		{"empty ref", func(m *cpv1.AppManifest) {}, "1.0.0", ""},
		{"private", func(m *cpv1.AppManifest) { m.Visibility = "private" }, "1.0.0", "r"},
		{"dup mount", func(m *cpv1.AppManifest) {
			m.Mounts = append(m.Mounts, &cpv1.ManifestMount{Name: "main", Path: "x"})
		}, "1.0.0", "r"},
		{"empty mount path", func(m *cpv1.AppManifest) { m.Mounts[0].Path = "" }, "1.0.0", "r"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := validManifest()
			c.mutate(m)
			if err := validateManifest(m, c.version, c.ref); err == nil {
				t.Fatalf("%s: expected error, got nil", c.name)
			}
		})
	}
	m := validManifest()
	m.Mounts = nil
	if err := validateManifest(m, "1.0.0", "r"); err != nil {
		t.Fatalf("storage-less app rejected: %v", err)
	}
}
