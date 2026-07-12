package fakepod_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"spawnery/internal/runtime/fakepod"
)

func TestPauseUnpauseStateMachine(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")

	if err := b.Unpause(ctx, h); !errors.Is(err, fakepod.ErrNotPaused) {
		t.Fatalf("Unpause of a running agent = %v, want ErrNotPaused", err)
	}
	if err := b.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if got := b.State("sp1", "agent"); got != fakepod.StatePaused {
		t.Fatalf("agent = %s, want paused", got)
	}
	if err := b.Pause(ctx, h); err == nil {
		t.Fatal("double Pause must fail, got nil")
	}
	if err := b.Unpause(ctx, h); err != nil {
		t.Fatalf("Unpause: %v", err)
	}
	if got := b.State("sp1", "agent"); got != fakepod.StateRunning {
		t.Fatalf("agent = %s, want running", got)
	}
	if b.PauseCount() != 2 || b.UnpauseCount() != 2 {
		t.Fatalf("Pause/Unpause counts = %d/%d, want 2/2 (attempts, including failures)",
			b.PauseCount(), b.UnpauseCount())
	}
}

func TestRestoreForkedSourceIsAnUnpause(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")
	if err := b.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := b.RestoreForkedSource(ctx, h); err != nil {
		t.Fatalf("RestoreForkedSource: %v", err)
	}
	if got := b.State("sp1", "agent"); got != fakepod.StateRunning {
		t.Fatalf("agent = %s, want running", got)
	}
	// On the early-failure path (the source was never paused) it returns the tolerable ErrNotPaused.
	if err := b.RestoreForkedSource(ctx, h); !errors.Is(err, fakepod.ErrNotPaused) {
		t.Fatalf("RestoreForkedSource of a running agent = %v, want ErrNotPaused", err)
	}
}

func TestAgentWriterFreezesWhilePausedAndUnpauseBlocksUntilItResumes(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")

	w, err := b.StartAgentWriter("sp1", fakepod.WriterInterval(200*time.Microsecond))
	if err != nil {
		t.Fatalf("StartAgentWriter: %v", err)
	}
	t.Cleanup(w.Stop)

	deadline := time.Now().Add(5 * time.Second)
	for w.Ticks() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("writer made no progress while the agent was running")
		}
		time.Sleep(time.Millisecond)
	}

	if err := b.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	frozen := w.Ticks()
	time.Sleep(20 * time.Millisecond)
	if got := w.Ticks(); got != frozen {
		t.Fatalf("writer ticked %d -> %d while paused; a paused agent must be frozen", frozen, got)
	}
	// While quiesced the two views agree — that is what a consistent snapshot pair looks like.
	root := b.RootfsView("sp1")
	mnt := b.MountView("sp1")
	if string(root["/work/.agent-seq"]) != string(mnt["/data/.agent-seq"]) {
		t.Fatalf("views disagree while quiesced: rootfs=%q mount=%q",
			root["/work/.agent-seq"], mnt["/data/.agent-seq"])
	}

	// Unpause BLOCKS until the resumed writer lands one write — the determinism hook (design note 7).
	if err := b.Unpause(ctx, h); err != nil {
		t.Fatalf("Unpause: %v", err)
	}
	if got := w.Ticks(); got <= frozen {
		t.Fatalf("Unpause returned before the writer resumed: ticks %d, want > %d", got, frozen)
	}
}

func TestStopJoinsTheWriter(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")
	w, err := b.StartAgentWriter("sp1", fakepod.WriterInterval(100*time.Microsecond))
	if err != nil {
		t.Fatalf("StartAgentWriter: %v", err)
	}
	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	w.Stop() // idempotent
	before := w.Ticks()
	time.Sleep(10 * time.Millisecond)
	if w.Ticks() != before {
		t.Fatal("writer still ticking after Stop")
	}
}

// The pair a suspend must produce: rootfs artifact and mount snapshot from the SAME instant. This is
// the primitive SE2's regression test (sp-2tx8.1.5) is built on.
func TestQuiescedCaptureIsNotTorn(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New(fakepod.WithBaseImage("agent:base", 3))
	t.Cleanup(b.Close)
	h := captureHandle(t, b, "sp1")
	w, err := b.StartAgentWriter("sp1", fakepod.WriterInterval(200*time.Microsecond))
	if err != nil {
		t.Fatalf("StartAgentWriter: %v", err)
	}
	t.Cleanup(w.Stop)

	if err := b.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	mountSnap := b.MountView("sp1") // the journaler's snapshot, taken while quiesced
	ref, err := b.CaptureDelta(ctx, h)
	if err != nil {
		t.Fatalf("CaptureDelta: %v", err)
	}
	rootfsSnap, _ := b.ImageContent(ref)
	if string(rootfsSnap["/work/.agent-seq"]) != string(mountSnap["/data/.agent-seq"]) {
		t.Fatalf("torn snapshot: rootfs seq %q != mount seq %q",
			rootfsSnap["/work/.agent-seq"], mountSnap["/data/.agent-seq"])
	}
}
