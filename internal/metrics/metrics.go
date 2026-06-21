// Package metrics owns the process-wide Prometheus registry and the /metrics HTTP handler.
//
// All binaries (spawnery_cp, spawnlet, sidecar) mount Handler() at /metrics to expose
// Go runtime and process metrics. Application-specific collectors are registered on
// Registry by their respective packages.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is the single process-wide Prometheus registry. All application metrics
// must be registered here (never on prometheus.DefaultRegisterer).
var Registry = prometheus.NewRegistry()

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// Handler returns an http.Handler that serves the Prometheus metrics from Registry
// in the text exposition format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{})
}
