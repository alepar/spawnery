package fakepod_test

import (
	"context"
	"testing"

	"spawnery/internal/runtime/fakepod"
)

func TestAgentWritePartitionsRootfsAndMounts(t *testing.T) {
	b := fakepod.New()
	t.Cleanup(b.Close)
	startPod(t, b, "sp1") // the agent has one mount at /data

	if err := b.AgentWrite("sp1", "/work/main.go", []byte("package main")); err != nil {
		t.Fatalf("AgentWrite rootfs: %v", err)
	}
	if err := b.AgentWrite("sp1", "/data/repo/file.txt", []byte("mounted")); err != nil {
		t.Fatalf("AgentWrite mount: %v", err)
	}

	root := b.RootfsView("sp1")
	if string(root["/work/main.go"]) != "package main" {
		t.Fatalf("rootfs = %v, want /work/main.go", root)
	}
	if _, ok := root["/data/repo/file.txt"]; ok {
		t.Fatal("a write under a mount path must NOT land in the rootfs view")
	}
	mnt := b.MountView("sp1")
	if string(mnt["/data/repo/file.txt"]) != "mounted" {
		t.Fatalf("mount view = %v, want /data/repo/file.txt", mnt)
	}
	if _, ok := mnt["/work/main.go"]; ok {
		t.Fatal("a rootfs write must NOT land in the mount view")
	}
}

func TestAgentWriteRequiresRunningAgent(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")
	if err := b.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// A paused agent is frozen: its processes cannot write. That is the property the suspend gate
	// depends on for a consistent snapshot.
	if err := b.AgentWrite("sp1", "/work/x", []byte("x")); err == nil {
		t.Fatal("AgentWrite on a paused agent must fail, got nil")
	}
}

func TestExecFailsOnPausedContainer(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")

	if err := b.Exec(ctx, h.AgentID, []string{"rm", "-rf", "/tmp"}); err != nil {
		t.Fatalf("Exec on a running agent: %v", err)
	}
	if err := b.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// This is the whole reason the buggy suspend teardown unpauses (SE2 §1): docker exec refuses.
	if err := b.Exec(ctx, h.AgentID, []string{"rm", "-rf", "/tmp"}); err == nil {
		t.Fatal("Exec on a paused container must fail, got nil")
	}
}

func TestScrubDeletesFromBothViews(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")
	for path, data := range map[string]string{
		"/tmp/junk":      "junk",
		"/work/keep":     "keep",
		"/data/precious": "user data",
	} {
		if err := b.AgentWrite("sp1", path, []byte(data)); err != nil {
			t.Fatalf("AgentWrite %s: %v", path, err)
		}
	}

	// The scrub is deliberately NOT mount-aware: a scrub that eats a journaled mount is a real bug
	// (SE2 §4.2), and the guard against it lives in the Manager. The fake must faithfully destroy,
	// or that guard has no failing test.
	if err := b.ScrubFn()(ctx, h.AgentID, []string{"/tmp", "/data"}); err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if _, ok := b.RootfsView("sp1")["/tmp/junk"]; ok {
		t.Fatal("scrub must delete /tmp/junk from the rootfs view")
	}
	if _, ok := b.RootfsView("sp1")["/work/keep"]; !ok {
		t.Fatal("scrub must not delete /work/keep")
	}
	if _, ok := b.MountView("sp1")["/data/precious"]; ok {
		t.Fatal("scrub of /data must delete mount content — the fake must be able to destroy it")
	}
}
