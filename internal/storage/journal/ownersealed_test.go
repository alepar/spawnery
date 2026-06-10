package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fixedCustody is a test PasswordProvider that always serves a constant password
// — it stands in for the ORIGIN node's node-local custody so a second
// (owner-sealed) Manager can be pointed at the SAME blob backend to simulate a
// cross-node resume.
type fixedCustody struct{ pw string }

func (c fixedCustody) PasswordFor(string) (string, error) { return c.pw, nil }
func (c fixedCustody) Forget(string) error                { return nil }

// TestOwnerSealedCustodyDeliveryAndError covers the core custody contract:
// PasswordFor errors before delivery and returns the delivered password after.
func TestOwnerSealedCustodyDeliveryAndError(t *testing.T) {
	c := NewOwnerSealedCustody()
	const spawn = "spawn-1"

	if _, err := c.PasswordFor(spawn); !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("PasswordFor before delivery = %v, want ErrNotDelivered", err)
	}
	if _, ok := c.Delivered(spawn); ok {
		t.Fatal("Delivered before delivery must be false")
	}

	if err := c.Deliver(spawn, 5, "repo-pw-abc"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	got, err := c.PasswordFor(spawn)
	if err != nil || got != "repo-pw-abc" {
		t.Fatalf("PasswordFor after delivery = %q, %v; want repo-pw-abc", got, err)
	}
	if gen, ok := c.Delivered(spawn); !ok || gen != 5 {
		t.Fatalf("Delivered = (%d, %v); want (5, true)", gen, ok)
	}

	// Empty password is rejected.
	if err := c.Deliver(spawn, 6, ""); err == nil {
		t.Fatal("Deliver with empty password must error")
	}

	// Forget clears it.
	if err := c.Forget(spawn); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := c.PasswordFor(spawn); !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("PasswordFor after Forget = %v, want ErrNotDelivered", err)
	}
}

// TestOwnerSealedCustodyGenerationFencing verifies the delivery is generation-
// fenced: a stale (older-gen) delivery is rejected and does not clobber the live
// key; a same-or-newer gen supersedes.
func TestOwnerSealedCustodyGenerationFencing(t *testing.T) {
	c := NewOwnerSealedCustody()
	const spawn = "spawn-fence"

	if err := c.Deliver(spawn, 7, "pw-gen7"); err != nil {
		t.Fatal(err)
	}
	// Stale gen rejected; live key unchanged.
	if err := c.Deliver(spawn, 6, "pw-gen6-STALE"); err == nil {
		t.Fatal("stale-gen delivery must be rejected")
	}
	if got, _ := c.PasswordFor(spawn); got != "pw-gen7" {
		t.Fatalf("stale delivery clobbered live key: got %q", got)
	}
	// Newer gen supersedes.
	if err := c.Deliver(spawn, 8, "pw-gen8"); err != nil {
		t.Fatalf("newer-gen delivery: %v", err)
	}
	if got, _ := c.PasswordFor(spawn); got != "pw-gen8" {
		t.Fatalf("newer delivery not applied: got %q", got)
	}
	// Same gen re-delivery (idempotent resume retry) is allowed.
	if err := c.Deliver(spawn, 8, "pw-gen8b"); err != nil {
		t.Fatalf("same-gen re-delivery: %v", err)
	}
}

// TestOwnerSealedCustodyWaitDelivered verifies WaitDelivered returns immediately
// when already delivered, blocks then wakes on delivery, and respects ctx.
func TestOwnerSealedCustodyWaitDelivered(t *testing.T) {
	c := NewOwnerSealedCustody()

	// Already delivered -> immediate.
	if err := c.Deliver("ready", 1, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitDelivered(context.Background(), "ready"); err != nil {
		t.Fatalf("WaitDelivered(already): %v", err)
	}

	// Blocks then wakes on a later delivery.
	var wg sync.WaitGroup
	wg.Add(1)
	woke := make(chan error, 1)
	go func() {
		defer wg.Done()
		woke <- c.WaitDelivered(context.Background(), "pending")
	}()
	// Give the waiter a moment to register, then deliver.
	time.Sleep(20 * time.Millisecond)
	if err := c.Deliver("pending", 1, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := <-woke; err != nil {
		t.Fatalf("WaitDelivered woke with error: %v", err)
	}
	wg.Wait()

	// ctx cancellation returns the ctx error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.WaitDelivered(ctx, "never"); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitDelivered(cancelled) = %v, want context.Canceled", err)
	}
}

// TestCrossNodeResumeOpensRepoWithDeliveredKey is the load-bearing cross-node
// proof: an ORIGIN manager writes a snapshot to a SHARED blob backend under a
// known repo password; a second manager on a "different node" (its own local
// repo config dir, an OwnerSealed custody, NO node-local seal for the spawn)
// can restore the snapshot ONLY after the password is delivered — and a node
// WITHOUT the delivered key cannot open the repo at all.
func TestCrossNodeResumeOpensRepoWithDeliveredKey(t *testing.T) {
	ctx := context.Background()
	const spawn = "spawn-x"
	const repoPW = "cross-node-repo-password-0123456789abcdef"

	// Shared blob backend = the Garage bucket both nodes see.
	sharedBlobs := t.TempDir()
	backend := &FilesystemBackend{Root: sharedBlobs}

	// --- ORIGIN node: snapshot a mount under the known password. ---
	originRoot := t.TempDir()
	origin, err := NewManager(Config{
		RepoRoot: filepath.Join(originRoot, "repos"),
		Backend:  backend,
		Custody:  fixedCustody{pw: repoPW},
	})
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	writeFile(t, src, "code.txt", "the-journaled-work")
	mt := Mount{Name: "work", HostDir: src, Class: OwnerSealed}
	ids, err := origin.FinalSnapshot(ctx, spawn, 1, []Mount{mt})
	if err != nil {
		t.Fatalf("origin snapshot: %v", err)
	}
	pin := ids["work"]
	if pin == "" {
		t.Fatal("origin produced no manifest")
	}
	if err := origin.Close(ctx, spawn); err != nil {
		t.Fatal(err)
	}

	// --- TARGET node WITHOUT the delivered key: cannot open the repo. ---
	targetRoot := t.TempDir()
	osc := NewOwnerSealedCustody()
	target, err := NewManager(Config{
		RepoRoot:    filepath.Join(targetRoot, "repos"),
		Backend:     backend, // same shared blobs
		Custody:     fixedCustody{pw: "WRONG-node-local-pw-that-must-not-be-used"},
		OwnerSealed: osc,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The resume target knows (from the CP) this is an owner-sealed spawn: mark it
	// so the journaler routes to the delivered key and never to a node-local mint.
	target.MarkOwnerSealed(spawn)
	if err := target.Restore(ctx, spawn, "work", pin, t.TempDir()); !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("restore before delivery = %v, want ErrNotDelivered (node without key cannot open repo)", err)
	}

	// --- Deliver the CORRECT password, then restore succeeds. ---
	if err := osc.Deliver(spawn, 1, repoPW); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := target.Restore(ctx, spawn, "work", pin, dst); err != nil {
		t.Fatalf("restore after delivery: %v", err)
	}
	if got := readFile(t, dst, "code.txt"); got != "the-journaled-work" {
		t.Fatalf("restored content = %q, want the-journaled-work", got)
	}
}

// TestWrongDeliveredPasswordCannotOpenRepo verifies that delivering the WRONG
// password (an attacker / corrupt key) fails the repo open rather than silently
// producing garbage.
func TestWrongDeliveredPasswordCannotOpenRepo(t *testing.T) {
	ctx := context.Background()
	const spawn = "spawn-wrong"
	const repoPW = "the-real-repo-password-abcdef0123456789"

	sharedBlobs := t.TempDir()
	backend := &FilesystemBackend{Root: sharedBlobs}

	origin, err := NewManager(Config{
		RepoRoot: filepath.Join(t.TempDir(), "repos"),
		Backend:  backend,
		Custody:  fixedCustody{pw: repoPW},
	})
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	writeFile(t, src, "f.txt", "x")
	ids, err := origin.FinalSnapshot(ctx, spawn, 1, []Mount{{Name: "work", HostDir: src, Class: OwnerSealed}})
	if err != nil {
		t.Fatal(err)
	}
	pin := ids["work"]
	_ = origin.Close(ctx, spawn)

	osc := NewOwnerSealedCustody()
	if err := osc.Deliver(spawn, 1, "a-different-wrong-password-zzzzzzzzzzzzzz"); err != nil {
		t.Fatal(err)
	}
	target, err := NewManager(Config{
		RepoRoot:    filepath.Join(t.TempDir(), "repos"),
		Backend:     backend,
		Custody:     fixedCustody{pw: "unused"},
		OwnerSealed: osc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Restore(ctx, spawn, "work", pin, t.TempDir()); err == nil {
		t.Fatal("restore with WRONG delivered password must fail to open the repo")
	}
}

// TestNodeLocalMountsUseNodeLocalCustodyUnchanged verifies that when an
// OwnerSealed custody is also configured, a spawn for which NO key was delivered
// still routes to the node-local default custody (the §4 origin / same-node
// path) and round-trips exactly as before.
func TestNodeLocalMountsUseNodeLocalCustodyUnchanged(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	keyfile := filepath.Join(root, "node.key")
	if err := GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatal(err)
	}
	nl, err := NewNodeLocalCustody(keyfile, filepath.Join(root, "seals"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Config{
		RepoRoot:    filepath.Join(root, "repos"),
		Backend:     &FilesystemBackend{Root: filepath.Join(root, "blobs")},
		Custody:     nl,
		OwnerSealed: NewOwnerSealedCustody(), // configured but NEVER delivered to
	})
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	writeFile(t, src, "n.txt", "node-local-data")
	mt := Mount{Name: "work", HostDir: src, Class: NodeLocal}
	ids, err := m.FinalSnapshot(ctx, "spawn-nl", 1, []Mount{mt})
	if err != nil {
		t.Fatalf("node-local final snapshot: %v", err)
	}
	pin := ids["work"]
	if pin == "" {
		t.Fatal("no manifest produced")
	}
	if err := m.Close(ctx, "spawn-nl"); err != nil {
		t.Fatal(err)
	}
	// Same-node resume restores from node-local custody with no delivery at all.
	dst := t.TempDir()
	if err := m.Restore(ctx, "spawn-nl", "work", pin, dst); err != nil {
		t.Fatalf("node-local restore (no delivery): %v", err)
	}
	if got := readFile(t, dst, "n.txt"); got != "node-local-data" {
		t.Fatalf("node-local restore = %q, want node-local-data", got)
	}

	// And the node-local seal file exists on disk — proving node-local custody was
	// used, not the (empty) owner-sealed one.
	if _, err := os.Stat(filepath.Join(root, "seals")); err != nil {
		t.Fatalf("expected node-local seal dir to exist: %v", err)
	}
}
