package cp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
	"spawnery/internal/metrics"
)

// TestNewServerWiresMetricsSources is the end-to-end acceptance check for the cp metrics wiring:
// NewServer wires cpmetrics.SetRegistrySource + SetSpawnStatusSource; seeding a registry and
// store then scraping /metrics should yield the spawnery_cp_spawns and node-class gauges.
//
// Not parallel: mutates process-global cpmetrics sources (via NewServer) and asserts gauge values.
func TestNewServerWiresMetricsSources(t *testing.T) {
	s, reg, _ := newTestServer(t)
	_ = s // server is built; sources are now wired to reg + its store

	// Add two cloud nodes: counts + free slots must appear in the scrape.
	reg.Add(&registry.Node{ID: "m1", Class: "cloud", Free: 3})
	reg.Add(&registry.Node{ID: "m2", Class: "cloud", Free: 5})

	// Create spawns in the store seeded by newTestServer. Create requires an app version; Seed()
	// in newTestServer plants secret-app/1.0.0 with mount "main". Drive two to Active, one stays Starting.
	ctx := context.Background()
	st := s.st

	createSpawnForMetrics := func(id string) {
		t.Helper()
		mounts := []store.Mount{{Name: "main", BackendURI: "scratch"}}
		if err := st.WithTx(ctx, func(tx store.Store) error {
			return tx.Spawns().Create(ctx, store.Spawn{
				ID: id, OwnerID: "alice", AppID: "secret-app", AppVersion: "1.0.0", AppRef: "examples/secret-app",
				Model: "m", Status: store.Starting, CreatedAt: 1, LastUsedAt: 1,
			}, mounts)
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	createSpawnForMetrics("sp-m1")
	createSpawnForMetrics("sp-m2")
	createSpawnForMetrics("sp-m3")

	if err := st.Spawns().SetActive(ctx, "sp-m1", "n1", 1); err != nil {
		t.Fatalf("SetActive sp-m1: %v", err)
	}
	if err := st.Spawns().SetActive(ctx, "sp-m2", "n1", 1); err != nil {
		t.Fatalf("SetActive sp-m2: %v", err)
	}
	// sp-m3 remains in Starting.

	// Scrape the /metrics endpoint.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metrics.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("metrics handler returned %d; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Assert node gauges.
	if !strings.Contains(body, `spawnery_cp_nodes_attached{class="cloud"} 2`) {
		t.Errorf("expected spawnery_cp_nodes_attached{class=\"cloud\"} 2 in:\n%s", body)
	}
	if !strings.Contains(body, `spawnery_cp_node_free_slots{class="cloud"} 8`) {
		t.Errorf("expected spawnery_cp_node_free_slots{class=\"cloud\"} 8 in:\n%s", body)
	}

	// Assert spawn status gauges.
	if !strings.Contains(body, `spawnery_cp_spawns{status="active"} 2`) {
		t.Errorf("expected spawnery_cp_spawns{status=\"active\"} 2 in:\n%s", body)
	}
	if !strings.Contains(body, `spawnery_cp_spawns{status="starting"} 1`) {
		t.Errorf("expected spawnery_cp_spawns{status=\"starting\"} 1 in:\n%s", body)
	}
}
