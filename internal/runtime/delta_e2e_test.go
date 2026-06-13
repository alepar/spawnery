//go:build delta_e2e

// Real-daemon integration check for delta-only export/import on the Docker lane (sp-ei4.1.14).
// Needs a reachable Docker daemon and two pre-seeded images (created by the shell prelude in the
// just recipe / runbook): SPIKE_BASE (base) and SPIKE_DELTA (= base + one committed top layer
// containing a deletion-as-whiteout, an added file, and an installed package).
//
//	docker build ... -t spike/base:v1          # base with /base-only.txt
//	docker commit  ... spike/delta:probe        # rm /base-only.txt; +sl; +/delta-added.txt
//	SPIKE_BASE=spike/base:v1 SPIKE_DELTA=spike/delta:probe \
//	  go test -tags delta_e2e ./internal/runtime/ -run TestDeltaOnlyRoundTripDockerLane -v
//
// It exports ONLY the top layer, removes the assembled delta image (so the target has the base
// only), reassembles base+delta, and leaves SPIKE_OUT for the shell to run+verify.
package runtime

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestDeltaOnlyRoundTripDockerLane(t *testing.T) {
	base := os.Getenv("SPIKE_BASE")
	delta := os.Getenv("SPIKE_DELTA")
	out := os.Getenv("SPIKE_OUT")
	if base == "" || delta == "" || out == "" {
		t.Skip("set SPIKE_BASE, SPIKE_DELTA, SPIKE_OUT to run the real-daemon delta round-trip")
	}
	rt, err := NewDocker()
	if err != nil {
		t.Fatalf("NewDocker: %v", err)
	}
	ctx := context.Background()

	// 1. Export ONLY the top (writable) layer as an uncompressed tar.
	var layer bytes.Buffer
	if err := rt.ExportTopLayer(ctx, delta, &layer); err != nil {
		t.Fatalf("ExportTopLayer: %v", err)
	}
	if layer.Len() == 0 {
		t.Fatal("exported layer is empty")
	}
	if bytes.HasPrefix(layer.Bytes(), []byte{0x1f, 0x8b}) {
		t.Fatal("exported layer is gzip; must be uncompressed for Kopia CDC dedup")
	}
	t.Logf("exported top layer: %d bytes (uncompressed tar)", layer.Len())

	// 2. Reassemble base + the shipped layer onto SPIKE_OUT (target has only the base).
	if err := rt.AssembleOnBase(ctx, base, out, bytes.NewReader(layer.Bytes())); err != nil {
		t.Fatalf("AssembleOnBase: %v", err)
	}
	if _, ok, err := rt.InspectImage(ctx, out); err != nil || !ok {
		t.Fatalf("reassembled image %s not present: ok=%v err=%v", out, ok, err)
	}
	t.Logf("reassembled %s — run it to verify whiteout/pkg/uid fidelity", out)
}
