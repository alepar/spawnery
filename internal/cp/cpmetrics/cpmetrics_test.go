// Package cpmetrics is the internal test package: we access unexported vars directly so we
// can snapshot counter values and compare deltas without exporting the vecs.
//
// IMPORTANT: These tests are NOT t.Parallel() because SetRegistrySource / SetSpawnStatusSource
// mutate process-global state, and the counter assertions use deltas (not absolutes) because
// counters accumulate across all tests in the same binary.
package cpmetrics

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeRegistry implements RegistrySource for tests.
type fakeRegistry struct {
	snap map[string]NodeClassStat
}

func (f *fakeRegistry) MetricsSnapshot() map[string]NodeClassStat { return f.snap }

// TestCounterHelpersIncrement verifies that the public counter helpers bump the right labels.
// Uses before/after deltas to tolerate counter accumulation across other tests in the binary.
func TestCounterHelpersIncrement(t *testing.T) {
	// Snapshot before (counters are monotonic and process-global).
	beforeSuccess := testutil.ToFloat64(placementsTotal.WithLabelValues("success"))
	beforeNoCapacity := testutil.ToFloat64(placementsTotal.WithLabelValues("no_capacity"))
	beforeClientBytes := testutil.ToFloat64(relayBytesTotal.WithLabelValues("from_client"))
	beforeNodeBytes := testutil.ToFloat64(relayBytesTotal.WithLabelValues("from_node"))
	beforeClientFrames := testutil.ToFloat64(relayFramesTotal.WithLabelValues("from_client"))
	beforeNodeFrames := testutil.ToFloat64(relayFramesTotal.WithLabelValues("from_node"))

	PlacementSuccess()
	PlacementNoCapacity()
	RelayFromClient(100)
	RelayFromNode(200)

	if d := testutil.ToFloat64(placementsTotal.WithLabelValues("success")) - beforeSuccess; d != 1 {
		t.Errorf("PlacementSuccess: delta = %v, want 1", d)
	}
	if d := testutil.ToFloat64(placementsTotal.WithLabelValues("no_capacity")) - beforeNoCapacity; d != 1 {
		t.Errorf("PlacementNoCapacity: delta = %v, want 1", d)
	}
	if d := testutil.ToFloat64(relayBytesTotal.WithLabelValues("from_client")) - beforeClientBytes; d != 100 {
		t.Errorf("RelayFromClient bytes: delta = %v, want 100", d)
	}
	if d := testutil.ToFloat64(relayBytesTotal.WithLabelValues("from_node")) - beforeNodeBytes; d != 200 {
		t.Errorf("RelayFromNode bytes: delta = %v, want 200", d)
	}
	if d := testutil.ToFloat64(relayFramesTotal.WithLabelValues("from_client")) - beforeClientFrames; d != 1 {
		t.Errorf("RelayFromClient frames: delta = %v, want 1", d)
	}
	if d := testutil.ToFloat64(relayFramesTotal.WithLabelValues("from_node")) - beforeNodeFrames; d != 1 {
		t.Errorf("RelayFromNode frames: delta = %v, want 1", d)
	}
}

// TestCollectorEmitsNodeGauges verifies the registry-snapshot gauge family.
// Not parallel: mutates process-global SetRegistrySource.
func TestCollectorEmitsNodeGauges(t *testing.T) {
	reg := &fakeRegistry{snap: map[string]NodeClassStat{
		"cloud": {Nodes: 2, FreeSlots: 5},
	}}
	SetRegistrySource(reg)
	SetSpawnStatusSource(nil) // unwire so spawn gauges don't interfere
	t.Cleanup(func() { SetRegistrySource(nil); SetSpawnStatusSource(nil) })

	want := `
# HELP spawnery_cp_node_free_slots Total free spawn slots across attached nodes, by class.
# TYPE spawnery_cp_node_free_slots gauge
spawnery_cp_node_free_slots{class="cloud"} 5
# HELP spawnery_cp_nodes_attached Nodes currently attached to the CP, by class.
# TYPE spawnery_cp_nodes_attached gauge
spawnery_cp_nodes_attached{class="cloud"} 2
`
	c := &cpCollector{}
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"spawnery_cp_nodes_attached", "spawnery_cp_node_free_slots"); err != nil {
		t.Errorf("node gauge mismatch:\n%v", err)
	}
}

// TestCollectorEmitsSpawnGauges verifies the spawn-status gauge family.
// Not parallel: mutates process-global SetSpawnStatusSource.
func TestCollectorEmitsSpawnGauges(t *testing.T) {
	SetRegistrySource(nil)
	SetSpawnStatusSource(func(_ context.Context) (map[string]int, error) {
		return map[string]int{"active": 3, "suspended": 1}, nil
	})
	t.Cleanup(func() { SetRegistrySource(nil); SetSpawnStatusSource(nil) })

	want := `
# HELP spawnery_cp_spawns Live (non-deleted) spawns by status.
# TYPE spawnery_cp_spawns gauge
spawnery_cp_spawns{status="active"} 3
spawnery_cp_spawns{status="suspended"} 1
`
	c := &cpCollector{}
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "spawnery_cp_spawns"); err != nil {
		t.Errorf("spawn gauge mismatch:\n%v", err)
	}
}

// TestCollectorNilSourcesAreHarmless verifies that unwired sources don't panic and emit nothing.
func TestCollectorNilSourcesAreHarmless(t *testing.T) {
	SetRegistrySource(nil)
	SetSpawnStatusSource(nil)
	t.Cleanup(func() { SetRegistrySource(nil); SetSpawnStatusSource(nil) })

	c := &cpCollector{}
	ch := make(chan prometheus.Metric, 16)
	c.Collect(ch)
	close(ch)
	var got []prometheus.Metric
	for m := range ch {
		got = append(got, m)
	}
	if len(got) != 0 {
		t.Errorf("nil sources must emit no metrics; got %d", len(got))
	}
}
