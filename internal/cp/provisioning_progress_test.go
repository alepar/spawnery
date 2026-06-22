package cp

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// feedStatusMsg sends a SpawnStatus NodeMessage into the in-channel and waits for the
// receive loop to process it (best-effort: polls provisioning map or waits a short time).
func feedStatusMsg(in chan *nodev1.NodeMessage, status *nodev1.SpawnStatus) {
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Status{Status: status}}
}

// waitCondition polls fn() until true or deadline exceeded.
func waitCondition(t *testing.T, label string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", label)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStatusHandlerUpdatesLiveProgress verifies that STARTING status messages with step_total>0
// update the provisioning map, and that STARTING with step_total==0 does NOT clobber (sp-m859.3).
func TestStatusHandlerUpdatesLiveProgress(t *testing.T) {
	s, reg, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	in := make(chan *nodev1.NodeMessage, 8)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	feedRegister(in, "n1", "")
	waitNodeClass(t, reg, "n1", "cloud")

	// Feed a real milestone event.
	feedStatusMsg(in, &nodev1.SpawnStatus{
		SpawnId:   "sp1",
		Phase:     nodev1.SpawnPhase_STARTING,
		StepIndex: 2, StepTotal: 7,
		StepKey: "prepare-mounts", StepLabel: "cloning repo",
	})

	// Wait for the provisioning map to reflect the update.
	waitCondition(t, "provisioning map set", func() bool {
		st, ok := s.provisioning.get("sp1")
		return ok && st.index == 2 && st.total == 7 && st.key == "prepare-mounts" && st.label == "cloning repo"
	})

	// ListSpawns should surface the progress.
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatalf("ListSpawns: %v", err)
	}
	var found *cpv1.SpawnSummary
	for _, sm := range resp.Msg.Spawns {
		if sm.SpawnId == "sp1" {
			found = sm
		}
	}
	if found == nil {
		t.Fatal("sp1 missing from ListSpawns")
	}
	if found.ProvisionStep != 2 || found.ProvisionTotal != 7 || found.ProvisionStepLabel != "cloning repo" {
		t.Fatalf("provision progress wrong: step=%d total=%d label=%q",
			found.ProvisionStep, found.ProvisionTotal, found.ProvisionStepLabel)
	}

	// A STARTING with StepTotal==0 must NOT clobber the existing progress.
	feedStatusMsg(in, &nodev1.SpawnStatus{
		SpawnId: "sp1",
		Phase:   nodev1.SpawnPhase_STARTING,
		// StepTotal == 0: no milestone data — must not overwrite.
	})
	// Give the loop a moment to process.
	time.Sleep(10 * time.Millisecond)
	st, ok := s.provisioning.get("sp1")
	if !ok || st.index != 2 || st.total != 7 {
		t.Fatalf("zero-total STARTING clobbered progress: ok=%v st=%+v", ok, st)
	}
}

// TestListSpawnsCarriesLiveAndPersisted verifies that ListSpawns surfaces both live
// provisioning progress (for STARTING) and persisted error fields (for Errored) (sp-m859.3).
func TestListSpawnsCarriesLiveAndPersisted(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	// Manually inject live progress.
	s.provisioning.set("sp1", provisionStep{index: 3, total: 5, key: "build-image", label: "building"})

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatalf("ListSpawns: %v", err)
	}
	var found *cpv1.SpawnSummary
	for _, sm := range resp.Msg.Spawns {
		if sm.SpawnId == "sp1" {
			found = sm
		}
	}
	if found == nil {
		t.Fatal("sp1 not in ListSpawns")
	}
	if found.ProvisionStep != 3 || found.ProvisionTotal != 5 {
		t.Fatalf("live provision fields: step=%d total=%d", found.ProvisionStep, found.ProvisionTotal)
	}
	if found.ErrorStep != "" || found.ErrorDetail != "" {
		t.Fatalf("error fields non-empty for Starting spawn: step=%q detail=%q", found.ErrorStep, found.ErrorDetail)
	}

	// Now SetError and verify persisted fields appear with zero live progress.
	if err := s.st.Spawns().SetError(ctx, "sp1", "create-pod", "boom"); err != nil {
		t.Fatalf("SetError: %v", err)
	}
	resp, err = s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatalf("ListSpawns after SetError: %v", err)
	}
	for _, sm := range resp.Msg.Spawns {
		if sm.SpawnId == "sp1" {
			found = sm
		}
	}
	if found.ErrorStep != "create-pod" || found.ErrorDetail != "boom" {
		t.Fatalf("persisted error fields: step=%q detail=%q", found.ErrorStep, found.ErrorDetail)
	}
	if found.ProvisionStep != 0 || found.ProvisionTotal != 0 || found.ProvisionStepLabel != "" {
		t.Fatalf("live provision fields non-zero for Errored spawn: %+v", found)
	}
}

// TestActiveStatusClearsLiveProgress verifies that an ACTIVE status message removes the
// spawn from the provisioning map (sp-m859.3).
func TestActiveStatusClearsLiveProgress(t *testing.T) {
	s, reg, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	// Pre-populate the live map.
	s.provisioning.set("sp1", provisionStep{index: 5, total: 5, key: "ready", label: "done"})

	in := make(chan *nodev1.NodeMessage, 8)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	feedRegister(in, "n1", "")
	waitNodeClass(t, reg, "n1", "cloud")

	// Feed ACTIVE — should clear the map.
	feedStatusMsg(in, &nodev1.SpawnStatus{
		SpawnId: "sp1",
		Phase:   nodev1.SpawnPhase_ACTIVE,
	})

	waitCondition(t, "provisioning map cleared on ACTIVE", func() bool {
		_, ok := s.provisioning.get("sp1")
		return !ok
	})
}

// TestRestartDropsLiveKeepsPersisted simulates a CP restart by constructing a fresh
// provisioningProgress (in-memory, empty) while the store row retains error_* values.
// ListSpawns must return persisted error fields and zero live progress (sp-m859.3).
func TestRestartDropsLiveKeepsPersisted(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	ctx := auth.WithOwner(context.Background(), "alice")

	// SetError persists to store.
	if err := s.st.Spawns().SetError(ctx, "sp1", "create-pod", "node died"); err != nil {
		t.Fatalf("SetError: %v", err)
	}

	// Simulate restart: replace the provisioning map with a fresh (empty) one.
	s.provisioning = newProvisioningProgress()

	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatalf("ListSpawns: %v", err)
	}
	var found *cpv1.SpawnSummary
	for _, sm := range resp.Msg.Spawns {
		if sm.SpawnId == "sp1" {
			found = sm
		}
	}
	if found == nil {
		t.Fatal("sp1 missing after simulated restart")
	}
	if found.ErrorStep != "create-pod" || found.ErrorDetail != "node died" {
		t.Fatalf("persisted error not surfaced after restart: step=%q detail=%q", found.ErrorStep, found.ErrorDetail)
	}
	if found.ProvisionStep != 0 || found.ProvisionTotal != 0 || found.ProvisionStepLabel != "" {
		t.Fatalf("stale live progress survived simulated restart: %+v", found)
	}
}

// TestProvisioningProgressMapConcurrency exercises the map under concurrent access (race detector).
func TestProvisioningProgressMapConcurrency(t *testing.T) {
	p := newProvisioningProgress()
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func(id int) {
			spawnID := "sp1"
			for j := 0; j < 100; j++ {
				p.set(spawnID, provisionStep{index: uint32(j), total: 10, key: "k", label: "l"})
				p.get(spawnID)
				p.clear(spawnID)
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}

// Ensure store.SetError accepts Errored store row with explicit step fields.
func TestListSpawnsErrorFieldsFromStore(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	ctx := auth.WithOwner(context.Background(), "alice")
	if err := s.st.Spawns().SetError(ctx, "sp1", "my-step", "my-detail"); err != nil {
		t.Fatalf("SetError: %v", err)
	}
	// Verify via Get.
	sp, err := s.st.Spawns().Get(ctx, "sp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sp.Status != store.Errored {
		t.Fatalf("status=%v want Errored", sp.Status)
	}
	if sp.ErrorStep != "my-step" || sp.ErrorDetail != "my-detail" {
		t.Fatalf("store fields: step=%q detail=%q", sp.ErrorStep, sp.ErrorDetail)
	}
}
