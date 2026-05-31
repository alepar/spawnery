package apps

import "testing"

func TestResolveKnownAndUnknown(t *testing.T) {
	r := New(map[string]string{"secret-app": "examples/secret-app"})
	ref, ok := r.Resolve("secret-app")
	if !ok || ref != "examples/secret-app" {
		t.Fatalf("known: got %q ok=%v", ref, ok)
	}
	if _, ok := r.Resolve("nope"); ok {
		t.Fatal("unknown should not resolve")
	}
}
