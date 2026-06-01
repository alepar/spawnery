package runtime

import "testing"

func TestBuildHostConfigLimits(t *testing.T) {
	h := buildHostConfig(ContainerSpec{
		MemoryBytes: 512 << 20, NanoCPUs: 1_500_000_000, PidsLimit: 200, Runtime: "runsc",
		Mounts: []Mount{{HostPath: "/h", ContainerPath: "/app", ReadOnly: true}},
	})
	if h.Resources.Memory != 512<<20 {
		t.Fatalf("Memory = %d", h.Resources.Memory)
	}
	if h.Resources.NanoCPUs != 1_500_000_000 {
		t.Fatalf("NanoCPUs = %d", h.Resources.NanoCPUs)
	}
	if h.Resources.PidsLimit == nil || *h.Resources.PidsLimit != 200 {
		t.Fatalf("PidsLimit = %v", h.Resources.PidsLimit)
	}
	if h.Runtime != "runsc" {
		t.Fatalf("Runtime = %q", h.Runtime)
	}
	if len(h.Binds) != 1 || h.Binds[0] != "/h:/app:ro" {
		t.Fatalf("Binds = %v", h.Binds)
	}
}

func TestBuildHostConfigHardening(t *testing.T) {
	h := buildHostConfig(ContainerSpec{DropAllCaps: true, ReadonlyRootfs: true})
	if len(h.CapDrop) != 1 || h.CapDrop[0] != "ALL" {
		t.Fatalf("CapDrop = %v (want [ALL])", h.CapDrop)
	}
	if !h.ReadonlyRootfs {
		t.Fatal("ReadonlyRootfs should be true")
	}
	if _, ok := h.Tmpfs["/tmp"]; !ok {
		t.Fatalf("expected /tmp tmpfs, got %v", h.Tmpfs)
	}
	// zero values -> nothing set
	h2 := buildHostConfig(ContainerSpec{})
	if len(h2.CapDrop) != 0 || h2.ReadonlyRootfs || h2.Tmpfs != nil {
		t.Fatalf("zero spec must not set hardening: capdrop=%v ro=%v tmpfs=%v", h2.CapDrop, h2.ReadonlyRootfs, h2.Tmpfs)
	}
}

func TestBuildHostConfigZeroValuesOmitted(t *testing.T) {
	h := buildHostConfig(ContainerSpec{NetnsOf: "sidecar123"})
	if h.Resources.Memory != 0 || h.Resources.NanoCPUs != 0 || h.Resources.PidsLimit != nil {
		t.Fatalf("zero limits must be unset: %+v", h.Resources)
	}
	if h.Runtime != "" {
		t.Fatalf("Runtime should be empty, got %q", h.Runtime)
	}
	if string(h.NetworkMode) != "container:sidecar123" {
		t.Fatalf("NetworkMode = %q", h.NetworkMode)
	}
}
