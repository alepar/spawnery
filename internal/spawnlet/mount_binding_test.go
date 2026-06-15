package spawnlet

import (
	"strings"
	"testing"

	"spawnery/internal/manifest"
)

func TestMountBindingsByNameRejectsEmptyBindingName(t *testing.T) {
	t.Parallel()

	_, err := mountBindingsByName([]manifest.Mount{{Name: ""}}, []MountBinding{{Name: "", BackendURI: "scratch:"}})
	if err == nil {
		t.Fatal("mountBindingsByName should reject empty binding names")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "empty") {
		t.Fatalf("mountBindingsByName error = %q, want empty-name detail", err)
	}
}
