// Package cpmetrics owns the control-plane Prometheus metrics.
//
// It is a leaf package: it imports only spawnery/internal/metrics and the prometheus client.
// Packages that own the data sources (registry, store) import cpmetrics for the shared types,
// not the reverse, so there is no import cycle.
//
// # Concurrency note for tests
//
// The counter vars (placementsTotal, relayBytesTotal, relayFramesTotal) and the gauge sources
// (registrySource, spawnStatusSource) are process-global. Tests that assert gauge values must
// wire their own source immediately before scraping and must NOT run t.Parallel(), because a
// concurrent test's SetRegistrySource call would swap the source mid-scrape. Counter assertions
// must use before/after deltas (testutil.ToFloat64) rather than absolute values, because counters
// accumulate monotonically across all tests in the same binary.
package cpmetrics

import (
	"context"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"spawnery/internal/metrics"
)

// NodeClassStat carries a per-class snapshot of the node registry.
type NodeClassStat struct {
	Nodes     int
	FreeSlots uint64
}

// RegistrySource is implemented by the node registry so cpmetrics can read a snapshot
// on each Prometheus scrape without importing the registry package (avoiding a cycle).
type RegistrySource interface {
	MetricsSnapshot() map[string]NodeClassStat
}

// package-level source pointers; nil = source not wired (collector emits nothing for that family).
var (
	registrySource    atomic.Pointer[RegistrySource]
	spawnStatusSource atomic.Pointer[func(context.Context) (map[string]int, error)]
)

var (
	placementsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spawnery_cp_placements_total",
		Help: "Spawn placement attempts by result (success or no_capacity).",
	}, []string{"result"})

	relayBytesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spawnery_cp_relay_bytes_total",
		Help: "Bytes relayed between clients and nodes by direction (from_client or from_node).",
	}, []string{"direction"})

	relayFramesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spawnery_cp_relay_frames_total",
		Help: "Frames relayed between clients and nodes by direction (from_client or from_node).",
	}, []string{"direction"})
)

// Desc vars for the snapshot gauges (pre-created so Collect doesn't allocate them per-scrape).
var (
	descNodesAttached = prometheus.NewDesc(
		"spawnery_cp_nodes_attached",
		"Nodes currently attached to the CP, by class.",
		[]string{"class"}, nil,
	)
	descNodeFreeSlots = prometheus.NewDesc(
		"spawnery_cp_node_free_slots",
		"Total free spawn slots across attached nodes, by class.",
		[]string{"class"}, nil,
	)
	descSpawns = prometheus.NewDesc(
		"spawnery_cp_spawns",
		"Live (non-deleted) spawns by status.",
		[]string{"status"}, nil,
	)
)

func init() {
	metrics.Registry.MustRegister(
		placementsTotal,
		relayBytesTotal,
		relayFramesTotal,
		&cpCollector{},
	)
}

// SetRegistrySource wires the node registry snapshot source. Pass nil to unwire (for tests).
// Idempotent; last-write-wins. Must be called before the first /metrics scrape for node gauges.
func SetRegistrySource(s RegistrySource) {
	if s == nil {
		registrySource.Store(nil)
	} else {
		registrySource.Store(&s)
	}
}

// SetSpawnStatusSource wires the spawn-status count source (adapter over SpawnRepo.CountByStatus).
// Pass nil to unwire (for tests). Idempotent; last-write-wins.
func SetSpawnStatusSource(fn func(context.Context) (map[string]int, error)) {
	if fn == nil {
		spawnStatusSource.Store(nil)
	} else {
		spawnStatusSource.Store(&fn)
	}
}

// PlacementSuccess records a successful placement (node selected, ACTIVE confirmed, route bound).
// Only Provision calls this — not PickNodeID, which is a non-committal pre-pick (counting it
// would double-count every Create/Resume that goes through the full two-phase flow).
func PlacementSuccess() { placementsTotal.WithLabelValues("success").Inc() }

// PlacementNoCapacity records a placement that failed because no eligible node had capacity.
func PlacementNoCapacity() { placementsTotal.WithLabelValues("no_capacity").Inc() }

// RelayFromClient records a frame and its byte count relayed from a client to a node.
func RelayFromClient(n int) {
	relayBytesTotal.WithLabelValues("from_client").Add(float64(n))
	relayFramesTotal.WithLabelValues("from_client").Inc()
}

// RelayFromNode records a frame and its byte count delivered from a node to a client.
// Only called when c != nil (the client is still attached); frames dropped for detached clients
// are intentionally uncounted — we track delivered/relayed throughput, not dropped frames.
// This means from_node may read lower than from_client; that is expected and documented.
func RelayFromNode(n int) {
	relayBytesTotal.WithLabelValues("from_node").Add(float64(n))
	relayFramesTotal.WithLabelValues("from_node").Inc()
}

// cpCollector is the prometheus.Collector for snapshot gauges.
// Describe emits nothing (unchecked collector): the label sets vary per scrape and are not
// known at registration time, so we skip duplicate-key checking in the registry.
type cpCollector struct{}

func (c *cpCollector) Describe(_ chan<- *prometheus.Desc) {}

func (c *cpCollector) Collect(ch chan<- prometheus.Metric) {
	if p := registrySource.Load(); p != nil {
		for class, stat := range (*p).MetricsSnapshot() {
			ch <- prometheus.MustNewConstMetric(descNodesAttached, prometheus.GaugeValue, float64(stat.Nodes), class)
			ch <- prometheus.MustNewConstMetric(descNodeFreeSlots, prometheus.GaugeValue, float64(stat.FreeSlots), class)
		}
	}
	if p := spawnStatusSource.Load(); p != nil {
		if counts, err := (*p)(context.Background()); err == nil {
			for status, n := range counts {
				ch <- prometheus.MustNewConstMetric(descSpawns, prometheus.GaugeValue, float64(n), status)
			}
		}
	}
}
