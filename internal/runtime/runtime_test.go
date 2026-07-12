package runtime

import (
	"context"
	"testing"
)

func TestFakeRecordsStartAndStop(t *testing.T) {
	f := NewFake()
	id, err := f.StartContainer(context.Background(), ContainerSpec{Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Started[0].Image; got != "img" {
		t.Fatalf("image not recorded: %q", got)
	}
	if err := f.StopContainer(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !f.Stopped[id] {
		t.Fatalf("stop not recorded for %s", id)
	}
}

func TestFakeContainerEnv(t *testing.T) {
	f := NewFake()
	id, err := f.StartContainer(context.Background(), ContainerSpec{Image: "img", Env: []string{"A=1", "B=2"}})
	if err != nil {
		t.Fatal(err)
	}

	env, err := f.ContainerEnv(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 2 || env[0] != "A=1" || env[1] != "B=2" {
		t.Fatalf("env = %v, want [A=1 B=2]", env)
	}

	if _, err := f.ContainerEnv(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("want error for unknown container id")
	}
}

func TestDockerDaemonBaseReferenceForBareImageID(t *testing.T) {
	const id = "sha256:fd391429fd7f342759418f0f3e552e0107c4e81e4124658b7752f9492f517ecc"

	ref, tagNeeded := dockerDaemonBaseReference(id)
	if !tagNeeded {
		t.Fatal("bare Docker image ID must be retagged before go-containerregistry daemon lookup")
	}
	if ref != "spawnery/local-base-id:fd391429fd7f342759418f0f3e552e0107c4e81e4124658b7752f9492f517ecc" {
		t.Fatalf("ref = %q", ref)
	}

	ref, tagNeeded = dockerDaemonBaseReference("spawnery/stubagent:dev")
	if tagNeeded || ref != "spawnery/stubagent:dev" {
		t.Fatalf("tag reference normalized to (%q, %v), want original/false", ref, tagNeeded)
	}

	ref, tagNeeded = dockerDaemonBaseReference("spawnery/stubagent@sha256:fd391429fd7f342759418f0f3e552e0107c4e81e4124658b7752f9492f517ecc")
	if tagNeeded || ref != "spawnery/stubagent@sha256:fd391429fd7f342759418f0f3e552e0107c4e81e4124658b7752f9492f517ecc" {
		t.Fatalf("repo digest normalized to (%q, %v), want original/false", ref, tagNeeded)
	}
}
