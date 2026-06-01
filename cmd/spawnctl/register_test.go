package main

import "testing"

func TestManifestToProto(t *testing.T) {
	pm, err := manifestToProto("../../examples/secret-app")
	if err != nil {
		t.Fatal(err)
	}
	if pm.ApiVersion != "spawnery/v1" || pm.Id != "spawnery/secret" || pm.Title != "Secret" {
		t.Fatalf("proto = %+v", pm)
	}
	if len(pm.Mounts) != 1 || pm.Mounts[0].Name != "main" || pm.Mounts[0].Path != "data" {
		t.Fatalf("mounts = %+v", pm.Mounts)
	}
}
